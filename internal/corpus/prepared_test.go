package corpus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// TestCorpusPrepared runs every corpus case (subsets m1-m3) twice, on fresh databases:
// once with COM_QUERY and once with COM_STMT_PREPARE/EXECUTE. Both must give the same
// error, the same column definitions and the same values (M4 T4; criterion 3 compares
// the types with the reference engine, which needs mysql-ref). The binary rows are
// decoded by go-mysql, independently of the server's encoder.
func TestCorpusPrepared(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "test", "corpus"), "")
	if err != nil {
		t.Fatal(err)
	}
	target := startMysqlite(t)
	ctx := context.Background()
	same, prepareRefused := 0, 0
	for i := range cases {
		c := &cases[i]
		text, textDB, textErr := runWith(ctx, target, c, false)
		bin, binDB, binErr := runWith(ctx, target, c, true)
		if d := compareProtocols(text, textErr, bin, binErr, textDB, binDB); d != "" {
			t.Errorf("%s (%s): %s", c.ID, c.File, d)
			continue
		}
		same++
		if textErr != nil {
			prepareRefused++
		}
	}
	t.Logf("%d cases: %d same through both protocols (%d of them the same error)", len(cases), same, prepareRefused)
}

// runWith runs the case's statement with the text protocol, or prepared and executed
// with no parameters.
func runWith(ctx context.Context, target Target, c *Case, prepared bool) (*mysql.Result, string, error) {
	db, err := target.Prepare(ctx, c)
	if err != nil {
		return nil, "", fmt.Errorf("setup: %w", err)
	}
	conn, err := target.Dial(db, c.FoundRows)
	if err != nil {
		return nil, "", fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	bind := func(s string) string { return strings.ReplaceAll(s, "{{db}}", db) }
	for _, s := range c.Session {
		if _, err := conn.Execute(bind(s)); err != nil {
			return nil, "", fmt.Errorf("session statement %q: %w", s, err)
		}
	}
	var res *mysql.Result
	if prepared {
		res, err = executePrepared(conn, bind(c.SQL))
	} else {
		res, err = conn.Execute(bind(c.SQL))
	}
	return res, db, err
}

func executePrepared(conn *client.Conn, q string) (*mysql.Result, error) {
	st, err := conn.Prepare(q)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if st.ParamNum() != 0 {
		return nil, fmt.Errorf("%d parameters in a corpus statement", st.ParamNum())
	}
	return st.Execute()
}

// compareProtocols compares the two runs; column names may contain the name of the
// run's database (textDB, binDB).
func compareProtocols(text *mysql.Result, textErr error, bin *mysql.Result, binErr error, textDB, binDB string) string {
	if textErr != nil || binErr != nil {
		var a, b *mysql.MyError
		if !errors.As(textErr, &a) || !errors.As(binErr, &b) {
			return fmt.Sprintf("text: %v; prepared: %v", textErr, binErr)
		}
		if a.Code != b.Code || a.State != b.State {
			return fmt.Sprintf("text error %d (%s), prepared error %d (%s): %s", a.Code, a.State, b.Code, b.State, b.Message)
		}
		return ""
	}
	if text.AffectedRows != bin.AffectedRows || text.InsertId != bin.InsertId {
		return fmt.Sprintf("affected/insert id: text %d/%d, prepared %d/%d", text.AffectedRows, text.InsertId, bin.AffectedRows, bin.InsertId)
	}
	if (text.Resultset == nil) != (bin.Resultset == nil) {
		return "a result set in one protocol only"
	}
	if text.Resultset == nil {
		return ""
	}
	if len(text.Fields) != len(bin.Fields) {
		return fmt.Sprintf("%d columns in text, %d prepared", len(text.Fields), len(bin.Fields))
	}
	for i, f := range text.Fields {
		g := bin.Fields[i]
		if strings.ReplaceAll(string(f.Name), textDB, "{{db}}") != strings.ReplaceAll(string(g.Name), binDB, "{{db}}") || f.Type != g.Type || f.Flag != g.Flag || f.Decimal != g.Decimal ||
			f.Charset != g.Charset || f.ColumnLength != g.ColumnLength {
			return fmt.Sprintf("column %d: text %s, prepared %s", i, fieldString(f), fieldString(g))
		}
	}
	rows, err := textRows(text.Resultset, nil)
	if err != nil {
		return err.Error()
	}
	if len(rows) != len(bin.Values) {
		return fmt.Sprintf("%d rows in text, %d prepared", len(rows), len(bin.Values))
	}
	for r := range text.Values {
		for i, f := range text.Fields {
			if d := sameValue(f, text.RowDatas[r], i, bin.Values[r][i]); d != "" {
				return fmt.Sprintf("row %d column %q: %s", r, f.Name, d)
			}
		}
	}
	return ""
}

func fieldString(f *mysql.Field) string {
	return fmt.Sprintf("%s %s flags %#x decimals %d charset %d length %d", f.Name, typeName(f.Type), f.Flag, f.Decimal, f.Charset, f.ColumnLength)
}

// sameValue compares column i of a text row with the value go-mysql decoded from the
// binary row: integers exactly, floats as numbers of their width, the rest as text.
func sameValue(f *mysql.Field, row mysql.RowData, i int, v mysql.FieldValue) string {
	raw, isNull := textColumn(row, i)
	if isNull || v.Type == mysql.FieldValueTypeNull {
		if isNull != (v.Type == mysql.FieldValueTypeNull) {
			return fmt.Sprintf("text %q (NULL %v), prepared %v", raw, isNull, v.Value())
		}
		return ""
	}
	var got string
	switch x := v.Value().(type) {
	case int64:
		got = strconv.FormatInt(x, 10)
	case uint64:
		got = strconv.FormatUint(x, 10)
	case float64:
		bits := 64
		if f.Type == mysql.MYSQL_TYPE_FLOAT {
			bits = 32
		}
		want, err := strconv.ParseFloat(raw, bits)
		if err != nil || x != want && !(math.IsNaN(x) && math.IsNaN(want)) {
			return fmt.Sprintf("text %q, prepared %v", raw, x)
		}
		return ""
	case []byte:
		got = string(x)
	default:
		got = fmt.Sprint(x)
	}
	if got != raw {
		return fmt.Sprintf("text %q, prepared %q", raw, got)
	}
	return ""
}

// textColumn returns column i of a text-protocol row.
func textColumn(row mysql.RowData, i int) (string, bool) {
	pos := 0
	for c := 0; ; c++ {
		v, isNull, n, _ := mysql.LengthEncodedString(row[pos:])
		if c == i {
			return string(v), isNull
		}
		pos += n
	}
}

package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/stmt"

	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// Prepared statements (ADR 0009): PREPARE finds the `?` markers and checks the
// statement with NULL in their place (0 for a LIMIT count); EXECUTE writes each bound
// value at its marker as a literal of its type and runs the text through COM_QUERY's
// pipeline, then encodes the result rows with the binary protocol.

// prepared is one prepared statement of the connection.
type prepared struct {
	query   string
	markers []dialect.Marker
}

// paramField is the definition sent for every parameter at PREPARE (oracle: pending).
var paramField = (&mysql.Field{Name: []byte("?"), Type: mysql.MYSQL_TYPE_VAR_STRING, Charset: meta.CharsetBinary}).Dump()

func (h *handler) HandleStmtPrepare(query string) (int, int, any, error) {
	seq := h.stmtSeq + 1
	h.stmtSeq = seq
	markers, e := h.tr.Markers(query)
	if e != nil {
		return 0, 0, nil, toMySQL(e)
	}
	cols, err := h.describeStatement(dialect.Substitute(query, markers, dialect.Placeholders(markers)))
	if err != nil {
		return 0, 0, nil, err
	}
	// go-mysql numbers statements by counting COM_STMT_PREPARE before calling the
	// handler (server/command.go, v1.16.0), failed ones included.
	ps := &stmt.PreparedStmt{ID: seq}
	for range markers {
		ps.RawParamFields = append(ps.RawParamFields, paramField)
	}
	for _, f := range cols {
		ps.RawColumnFields = append(ps.RawColumnFields, f.Dump())
	}
	if h.stmts == nil {
		h.stmts = map[*stmt.PreparedStmt]*prepared{}
	}
	h.stmts[ps] = &prepared{query: query, markers: markers}
	return len(markers), len(cols), ps, nil
}

// describeStatement translates the statement and returns the columns its result set
// will have, without running it; a SHOW or an EXPLAIN is run, as it has no effect.
func (h *handler) describeStatement(query string) ([]*mysql.Field, error) {
	current, _ := h.sess.Database()
	st, e := h.tr.TranslateIn(query, current, sessionCatalog{h})
	if e != nil {
		return nil, toMySQL(e)
	}
	if e := h.bindVariables(st); e != nil {
		return nil, toMySQL(e)
	}
	var res *mysql.Result
	var err error
	switch st.Kind {
	case dialect.KindRead:
		h.describe = true
		res, err = h.read(st)
		h.describe = false
	case dialect.KindShow:
		res, err = h.show(st.Show)
	}
	if err != nil || res == nil || res.Resultset == nil {
		return nil, err
	}
	return res.Fields, nil
}

func (h *handler) HandleStmtExecute(context any, _ string, args []any) (*mysql.Result, error) {
	res, err := h.execute(context, args)
	if err != nil {
		return nil, h.executeError(err)
	}
	return res, nil
}

// executeError sends EXECUTE's error itself: go-mysql wraps the handler's error with a
// stack trace, so its writer no longer sees a *mysql.MyError and sends 1105 with the
// message as text (ADR 0003, G8). The packet go-mysql then writes is dropped.
func (h *handler) executeError(err error) error {
	var my *mysql.MyError
	if !errors.As(err, &my) || h.conn == nil || h.filter == nil {
		return err
	}
	if werr := h.conn.WriteValue(my); werr != nil {
		return werr
	}
	h.filter.dropNext()
	return my
}

func (h *handler) execute(context any, args []any) (*mysql.Result, error) {
	ps := asPrepared(context)
	p := h.stmts[ps]
	if p == nil {
		// Released by COM_RESET_CONNECTION or COM_CHANGE_USER; go-mysql still knows it.
		id := uint32(0)
		if ps != nil {
			id = ps.ID
		}
		return nil, toMySQL(errs.UnknownStmtHandler(id, "mysqld_stmt_execute"))
	}
	values := make([]string, len(p.markers))
	for i, m := range p.markers {
		var e *errs.Error
		if m.Limit {
			values[i], e = limitLiteral(args[i])
		} else {
			values[i], e = paramLiteral(args[i], h.tr.NoBackslashEscapes())
		}
		if e != nil {
			return nil, toMySQL(e)
		}
	}
	res, err := h.run(dialect.Substitute(p.query, p.markers, values))
	if err != nil || res == nil || res.Resultset == nil {
		return res, err
	}
	if err := binaryResultset(res.Resultset); err != nil {
		return nil, h.fail(err, errs.Hint{})
	}
	return res, nil
}

func (h *handler) HandleStmtClose(context any) error {
	delete(h.stmts, asPrepared(context))
	return nil
}

func asPrepared(context any) *stmt.PreparedStmt {
	ps, _ := context.(*stmt.PreparedStmt)
	return ps
}

// decimalLiteral is the text a DECIMAL parameter must have to be written as is.
var decimalLiteral = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

// paramLiteral writes a bound parameter value as an SQL literal of its type: integers
// and floats as numbers, DECIMAL as its digits, dates and times as quoted canonical
// text, everything else as a quoted string.
func paramLiteral(v any, noBackslashEscapes bool) (string, *errs.Error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case int8:
		return signed(int64(x)), nil
	case int16:
		return signed(int64(x)), nil
	case int32:
		return signed(int64(x)), nil
	case int64:
		return signed(x), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return float(float64(x), 32)
	case float64:
		return float(x, 64)
	case []byte:
		return quote(x, noBackslashEscapes), nil
	case mysql.TypedBytes:
		switch x.Type {
		case mysql.MYSQL_TYPE_DECIMAL, mysql.MYSQL_TYPE_NEWDECIMAL:
			if !decimalLiteral.Match(x.Bytes) {
				return "", errs.WrongArguments("EXECUTE")
			}
			if x.Bytes[0] == '-' {
				return "(" + string(x.Bytes) + ")", nil
			}
			return string(x.Bytes), nil
		case mysql.MYSQL_TYPE_DATE, mysql.MYSQL_TYPE_NEWDATE, mysql.MYSQL_TYPE_DATETIME, mysql.MYSQL_TYPE_TIMESTAMP:
			s, ok := binaryDatetime(x.Bytes, x.Type == mysql.MYSQL_TYPE_DATE || x.Type == mysql.MYSQL_TYPE_NEWDATE)
			if !ok {
				return "", errs.WrongArguments("EXECUTE")
			}
			return "'" + s + "'", nil
		case mysql.MYSQL_TYPE_TIME:
			s, ok := binaryTime(x.Bytes)
			if !ok {
				return "", errs.WrongArguments("EXECUTE")
			}
			return "'" + s + "'", nil
		}
		return quote(x.Bytes, noBackslashEscapes), nil
	}
	return "", errs.WrongArguments("EXECUTE")
}

// signed writes a negative number in parentheses, so `-?` never becomes `--`.
func signed(n int64) string {
	if n < 0 {
		return "(" + strconv.FormatInt(n, 10) + ")"
	}
	return strconv.FormatInt(n, 10)
}

// float writes a FLOAT or DOUBLE parameter with an exponent, which MySQL reads as a
// DOUBLE literal (not a DECIMAL one).
func float(f float64, bits int) (string, *errs.Error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errs.WrongArguments("EXECUTE")
	}
	s := strconv.FormatFloat(f, 'e', -1, bits)
	if f < 0 {
		return "(" + s + ")", nil
	}
	return s, nil
}

// quote writes a string literal: a quote is doubled; with backslash escapes on, a
// backslash and NUL are escaped too.
func quote(b []byte, noBackslashEscapes bool) string {
	var s strings.Builder
	s.Grow(len(b) + 2)
	s.WriteByte('\'')
	for _, c := range b {
		switch {
		case c == '\'':
			s.WriteString("''")
		case c == '\\' && !noBackslashEscapes:
			s.WriteString(`\\`)
		case c == 0 && !noBackslashEscapes:
			s.WriteString(`\0`)
		default:
			s.WriteByte(c)
		}
	}
	s.WriteByte('\'')
	return s.String()
}

// limitLiteral is the value of a LIMIT or OFFSET marker: a non-negative integer, sent
// as an integer or as its decimal text. As in MySQL 8.0.46, a negative integer is 1690
// and NULL is 0; a fraction is 1210. Text is accepted when it is a decimal integer
// (MySQL's EXECUTE ... USING refuses a string; the binary protocol is not checked).
func limitLiteral(v any) (string, *errs.Error) {
	var n uint64
	switch x := v.(type) {
	case nil:
		return "0", nil
	case int8, int16, int32, int64:
		i := toInt64(x)
		if i < 0 {
			return "", errs.UnsignedOutOfRange("EXECUTE")
		}
		n = uint64(i)
	case uint8:
		n = uint64(x)
	case uint16:
		n = uint64(x)
	case uint32:
		n = uint64(x)
	case uint64:
		n = x
	case mysql.TypedBytes:
		u, err := strconv.ParseUint(string(x.Bytes), 10, 64)
		if err != nil {
			return "", errs.WrongArguments("EXECUTE")
		}
		n = u
	default:
		return "", errs.WrongArguments("EXECUTE")
	}
	return strconv.FormatUint(n, 10), nil
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	}
	return v.(int64)
}

// binaryDatetime decodes a binary-protocol DATE/DATETIME value (its length byte
// removed): 0, 4, 7 or 11 bytes.
func binaryDatetime(b []byte, dateOnly bool) (string, bool) {
	switch len(b) {
	case 0:
		if dateOnly {
			return "0000-00-00", true
		}
		return "0000-00-00 00:00:00", true
	case 4, 7, 11:
	default:
		return "", false
	}
	s := fmt.Sprintf("%04d-%02d-%02d", binary.LittleEndian.Uint16(b), b[2], b[3])
	if len(b) == 4 {
		return s, true
	}
	s += fmt.Sprintf(" %02d:%02d:%02d", b[4], b[5], b[6])
	if len(b) == 11 {
		s += fmt.Sprintf(".%06d", binary.LittleEndian.Uint32(b[7:]))
	}
	return s, true
}

// binaryTime decodes a binary-protocol TIME value: 0, 8 or 12 bytes.
func binaryTime(b []byte) (string, bool) {
	switch len(b) {
	case 0:
		return "00:00:00", true
	case 8, 12:
	default:
		return "", false
	}
	sign := ""
	if b[0] == 1 {
		sign = "-"
	}
	hours := uint64(binary.LittleEndian.Uint32(b[1:5]))*24 + uint64(b[5])
	s := fmt.Sprintf("%s%02d:%02d:%02d", sign, hours, b[6], b[7])
	if len(b) == 12 {
		s += fmt.Sprintf(".%06d", binary.LittleEndian.Uint32(b[8:]))
	}
	return s, true
}

// binaryResultset re-encodes text-protocol rows in the binary protocol, by the
// declared type of each field: COM_QUERY's pipeline builds every result set (SELECT,
// SHOW, EXPLAIN, information_schema) in the text protocol.
func binaryResultset(rs *mysql.Resultset) error {
	defs := make([]meta.ColumnDef, len(rs.Fields))
	for i, f := range rs.Fields {
		defs[i] = meta.ColumnDef{Name: string(f.Name), Type: f.Type, Flags: f.Flag}
	}
	for r, row := range rs.RowDatas {
		bin, err := textToBinary(defs, row)
		if err != nil {
			return err
		}
		rs.RowDatas[r] = bin
	}
	return nil
}

// textToBinary converts one text-protocol row (a length-encoded string or 0xfb for
// NULL per column) to a binary-protocol row.
func textToBinary(defs []meta.ColumnDef, row []byte) ([]byte, error) {
	bitmap := make([]byte, (len(defs)+7+2)/8)
	var values []byte
	pos := 0
	for i, d := range defs {
		if pos >= len(row) {
			return nil, fmt.Errorf("wire: text row ends at column %d of %d", i, len(defs))
		}
		if row[pos] == 0xfb {
			pos++
			b := i + 2
			bitmap[b/8] |= 1 << (b % 8)
			continue
		}
		s, _, n, err := mysql.LengthEncodedString(row[pos:])
		if err != nil {
			return nil, fmt.Errorf("wire: text row column %d: %w", i, err)
		}
		pos += n
		if values, err = appendBinary(values, d, s); err != nil {
			return nil, fmt.Errorf("wire: column %q: %w", d.Name, err)
		}
	}
	out := make([]byte, 0, 1+len(bitmap)+len(values))
	out = append(out, 0x00)
	out = append(out, bitmap...)
	return append(out, values...), nil
}

package corpus

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// typeCodes maps the type names a case may use to the protocol type codes.
var typeCodes = map[string]byte{
	"DECIMAL":     mysql.MYSQL_TYPE_DECIMAL,
	"TINY":        mysql.MYSQL_TYPE_TINY,
	"SHORT":       mysql.MYSQL_TYPE_SHORT,
	"LONG":        mysql.MYSQL_TYPE_LONG,
	"FLOAT":       mysql.MYSQL_TYPE_FLOAT,
	"DOUBLE":      mysql.MYSQL_TYPE_DOUBLE,
	"NULL":        mysql.MYSQL_TYPE_NULL,
	"TIMESTAMP":   mysql.MYSQL_TYPE_TIMESTAMP,
	"LONGLONG":    mysql.MYSQL_TYPE_LONGLONG,
	"INT24":       mysql.MYSQL_TYPE_INT24,
	"DATE":        mysql.MYSQL_TYPE_DATE,
	"TIME":        mysql.MYSQL_TYPE_TIME,
	"DATETIME":    mysql.MYSQL_TYPE_DATETIME,
	"YEAR":        mysql.MYSQL_TYPE_YEAR,
	"VARCHAR":     mysql.MYSQL_TYPE_VARCHAR,
	"BIT":         mysql.MYSQL_TYPE_BIT,
	"JSON":        mysql.MYSQL_TYPE_JSON,
	"NEWDECIMAL":  mysql.MYSQL_TYPE_NEWDECIMAL,
	"ENUM":        mysql.MYSQL_TYPE_ENUM,
	"SET":         mysql.MYSQL_TYPE_SET,
	"TINY_BLOB":   mysql.MYSQL_TYPE_TINY_BLOB,
	"MEDIUM_BLOB": mysql.MYSQL_TYPE_MEDIUM_BLOB,
	"LONG_BLOB":   mysql.MYSQL_TYPE_LONG_BLOB,
	"BLOB":        mysql.MYSQL_TYPE_BLOB,
	"VAR_STRING":  mysql.MYSQL_TYPE_VAR_STRING,
	"STRING":      mysql.MYSQL_TYPE_STRING,
	"GEOMETRY":    mysql.MYSQL_TYPE_GEOMETRY,
}

func typeName(code byte) string {
	for name, c := range typeCodes {
		if c == code {
			return name
		}
	}
	return fmt.Sprintf("0x%02x", code)
}

// Target is the server under test.
type Target interface {
	// Prepare creates a fresh database holding the case's fixture and returns its name.
	Prepare(ctx context.Context, c *Case) (string, error)
	// Dial connects to database db, sending CLIENT_FOUND_ROWS when foundRows is set.
	Dial(db string, foundRows bool) (*client.Conn, error)
}

// Status is the outcome of one case.
type Status string

// Statuses; a deviated case is never a pass (docs/07-testing.md section 3).
const (
	Passed   Status = "passed"
	Failed   Status = "failed"
	Deviated Status = "deviated"
)

// Result is the outcome of one case. Detail explains a failure.
type Result struct {
	Case   *Case
	Status Status
	Detail string
}

// Report counts the outcomes. Pending counts the passes whose expectation is
// `oracle: pending`: they match the hypothesis, not yet MySQL.
type Report struct {
	Results                           []Result
	Passed, Failed, Deviated, Pending int
}

// Summary is the human line of the report.
func (r *Report) Summary() string {
	return fmt.Sprintf("%d cases: %d passed (%d oracle: pending), %d failed, %d deviated",
		len(r.Results), r.Passed, r.Pending, r.Failed, r.Deviated)
}

// Run executes every case on t.
func Run(ctx context.Context, t Target, cases []Case) *Report {
	rep := &Report{}
	for i := range cases {
		c := &cases[i]
		detail := runCase(ctx, t, c)
		r := Result{Case: c, Status: Passed, Detail: detail}
		switch {
		case c.KnownDeviation != nil:
			r.Status = Deviated
			rep.Deviated++
		case detail != "":
			r.Status = Failed
			rep.Failed++
		default:
			rep.Passed++
			if c.Oracle == "pending" {
				rep.Pending++
			}
		}
		rep.Results = append(rep.Results, r)
	}
	return rep
}

// runCase returns "" when the case matches its expectation, else what differs.
func runCase(ctx context.Context, t Target, c *Case) string {
	res, err, problem := execute(ctx, t, c)
	if problem != "" {
		return problem
	}
	return compare(&c.Expect, res, err)
}

// execute prepares the case on t and runs its statement. problem is set when the case
// could not run at all (setup, connection or session statement); otherwise res and err
// are the statement's outcome.
func execute(ctx context.Context, t Target, c *Case) (res *mysql.Result, err error, problem string) {
	db, err := t.Prepare(ctx, c)
	var my *mysql.MyError
	if errors.As(err, &my) && my.Code == 1235 {
		// The engine refused the case's setup as unsupported: that is its answer to the
		// case (an unsupported construct), not a failure to run it.
		return nil, fmt.Errorf("setup: %w", err), ""
	}
	if err != nil {
		return nil, nil, "setup: " + err.Error()
	}
	conn, err := t.Dial(db, c.FoundRows)
	if err != nil {
		return nil, nil, "connect: " + err.Error()
	}
	defer conn.Close()
	// {{db}} is the case's database, whose name the target chooses.
	bind := func(s string) string { return strings.ReplaceAll(s, "{{db}}", db) }
	for _, s := range c.Session {
		if _, err := conn.Execute(bind(s)); err != nil {
			return nil, nil, fmt.Sprintf("session statement %q: %v", s, err)
		}
	}
	res, err = conn.Execute(bind(c.SQL))
	return res, err, ""
}

func compare(e *Expect, res *mysql.Result, err error) string {
	var diffs []string
	add := func(format string, args ...any) { diffs = append(diffs, fmt.Sprintf(format, args...)) }

	if e.Error != nil {
		var my *mysql.MyError
		switch {
		case err == nil:
			add("want error %d (%s), got success", e.Error.Code, e.Error.SQLState)
		case !errors.As(err, &my):
			add("want error %d (%s), got %v", e.Error.Code, e.Error.SQLState, err)
		case my.Code != e.Error.Code || my.State != e.Error.SQLState:
			add("want error %d (%s), got %d (%s): %s", e.Error.Code, e.Error.SQLState, my.Code, my.State, my.Message)
		}
		return strings.Join(diffs, "; ")
	}
	if err != nil {
		return "unexpected error: " + err.Error()
	}

	if e.AffectedRows != nil && res.AffectedRows != *e.AffectedRows {
		add("affected rows: want %d, got %d", *e.AffectedRows, res.AffectedRows)
	}
	if e.InsertID != nil && res.InsertId != *e.InsertID {
		add("insert id: want %d, got %d", *e.InsertID, res.InsertId)
	}
	if e.Info != nil && res.StatusMessage != *e.Info {
		add("info: want %q, got %q", *e.Info, res.StatusMessage)
	}

	if e.Rows != nil || e.Columns != nil || e.Types != nil {
		if res.Resultset == nil {
			return "want a result set, got OK"
		}
		fields := res.Resultset.Fields
		if e.Columns != nil {
			var names []string
			for _, f := range fields {
				names = append(names, string(f.Name))
			}
			if !equal(names, e.Columns) {
				add("columns: want %v, got %v", e.Columns, names)
			}
		}
		if e.Types != nil {
			var types []string
			for _, f := range fields {
				types = append(types, typeName(f.Type))
			}
			if !equal(types, e.Types) {
				add("types: want %v, got %v", e.Types, types)
			}
		}
		if e.Rows != nil {
			got, err := textRows(res.Resultset, e.Volatile)
			if err != nil {
				return err.Error()
			}
			want := render(*e.Rows)
			if !e.Ordered {
				sort.Strings(got)
				sort.Strings(want)
			}
			if !equal(got, want) {
				add("rows: want %v, got %v", want, got)
			}
		}
	}
	return strings.Join(diffs, "; ")
}

// textRows decodes the raw text-protocol rows, so values are compared exactly as sent
// (the client library reformats some types when it parses them).
// textRows renders each row; the value of a volatile column (named in any case) is
// rendered as "*".
func textRows(rs *mysql.Resultset, volatile []string) ([]string, error) {
	skip := map[int]bool{}
	for i, f := range rs.Fields {
		for _, v := range volatile {
			if strings.EqualFold(string(f.Name), v) {
				skip[i] = true
			}
		}
	}
	star := "*"
	var rows []string
	for i, raw := range rs.RowDatas {
		var vals []*string
		for pos := 0; pos < len(raw); {
			v, isNull, n, err := mysql.LengthEncodedString(raw[pos:])
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", i, err)
			}
			pos += n
			if skip[len(vals)] {
				vals = append(vals, &star)
				continue
			}
			if isNull {
				vals = append(vals, nil)
				continue
			}
			s := string(v)
			vals = append(vals, &s)
		}
		rows = append(rows, renderRow(vals))
	}
	return rows, nil
}

func render(rows [][]*string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, renderRow(r))
	}
	return out
}

// renderRow quotes values so NULL and the string "NULL" stay distinct.
func renderRow(vals []*string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		if v == nil {
			parts[i] = "NULL"
		} else {
			parts[i] = fmt.Sprintf("%q", *v)
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var autoIncrement = regexp.MustCompile(`(?i)\bAUTO_INCREMENT\b`)

// SQLiteFixture translates one fixture statement for a target that loads fixtures
// straight into SQLite, before mysqlite runs DDL (M2). Fixtures are written in the
// subset both engines accept; the only rewrite is `INTEGER PRIMARY KEY AUTO_INCREMENT`
// to SQLite's `AUTOINCREMENT`, which the catalog reads back as AUTO_INCREMENT.
func SQLiteFixture(stmt string) string {
	return autoIncrement.ReplaceAllString(stmt, "AUTOINCREMENT")
}

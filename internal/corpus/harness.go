package corpus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// The differential harness (docs/07-testing.md section 4): every case runs on the
// reference engine (mysql-ref) and on the system under test, from the same fixture
// statements, and the two outcomes are compared. The reference is the oracle: a case
// whose expectation disagrees with it is a wrong hypothesis (ADR 0002 addendum), and
// is reported apart from a mysqlite failure.

// Net is a Target reached over the network: mysql-ref or a running mysqlite. Each case
// gets a database of its own, created with CREATE DATABASE.
type Net struct {
	Addr, User, Password string
	prefix               string
	n                    int
	created              []string
}

// RunPrefix returns a random prefix for the case databases of one harness run, so runs
// against a server that keeps its data do not collide. Both engines must get the same
// prefix: a database name shows in results (SHOW TABLES, DATABASE()).
func RunPrefix() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "h" + hex.EncodeToString(b) + "_", nil
}

// NewNet returns a Net naming its case databases prefix0001, prefix0002, ...
func NewNet(addr, user, password, prefix string) *Net {
	return &Net{Addr: addr, User: user, Password: password, prefix: prefix}
}

// Prepare creates the case's database and runs its fixture through the protocol.
func (t *Net) Prepare(ctx context.Context, c *Case) (string, error) {
	t.n++
	name := fmt.Sprintf("%s%04d", t.prefix, t.n)
	conn, err := t.Dial("", false)
	if err != nil {
		return "", err
	}
	_, err = conn.Execute("CREATE DATABASE " + name)
	conn.Close()
	if err != nil {
		return "", err
	}
	t.created = append(t.created, name)
	conn, err = t.Dial(name, false)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	for _, s := range c.Setup {
		if _, err := conn.Execute(s); err != nil {
			return "", fmt.Errorf("%q: %w", s, err)
		}
	}
	return name, nil
}

// Dial connects to database db (none when db is ""). CLIENT_SESSION_TRACK is sent, as
// the mysql CLI does: go-mysql reads the OK packet's info string only with it.
func (t *Net) Dial(db string, foundRows bool) (*client.Conn, error) {
	return client.Connect(t.Addr, t.User, t.Password, db, func(c *client.Conn) error {
		flags := []uint32{mysql.CLIENT_SESSION_TRACK}
		if foundRows {
			flags = append(flags, mysql.CLIENT_FOUND_ROWS)
		}
		for _, f := range flags {
			if err := c.SetCapability(f); err != nil {
				return err
			}
		}
		return nil
	})
}

// Wait returns once the server accepts a connection, or the last error after timeout:
// in the compose topology the harness can start before the servers listen.
func (t *Net) Wait(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := t.Dial("", false)
		if err == nil {
			return conn.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: %w", t.Addr, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// DropAll drops the databases Prepare created; mysql-ref is cleaned this way. It stops
// at the first error.
func (t *Net) DropAll() error {
	conn, err := t.Dial("", false)
	if err != nil {
		return err
	}
	defer conn.Close()
	for len(t.created) > 0 {
		if _, err := conn.Execute("DROP DATABASE IF EXISTS " + t.created[0]); err != nil {
			return err
		}
		t.created = t.created[1:]
	}
	return nil
}

// Outcome is what one engine answered to a case, in comparable form. Problem is set
// when the case could not run (fixture, connection, a non-MySQL error).
type Outcome struct {
	Problem      string   `json:"problem,omitempty"`
	Error        *Error   `json:"error,omitempty"`
	Message      string   `json:"message,omitempty"`
	Columns      []string `json:"columns,omitempty"`
	Types        []string `json:"types,omitempty"`
	Rows         []string `json:"rows,omitempty"`
	AffectedRows uint64   `json:"affected_rows"`
	InsertID     uint64   `json:"insert_id"`
	Info         string   `json:"info,omitempty"`
}

// outcome normalizes a statement's result: rows are sorted unless the case compares
// them in order, NULL stays distinct from the string "NULL" (renderRow).
func outcome(c *Case, res *mysql.Result, err error, problem string) Outcome {
	if problem != "" {
		return Outcome{Problem: problem}
	}
	if err != nil {
		var my *mysql.MyError
		if errors.As(err, &my) {
			return Outcome{Error: &Error{Code: my.Code, SQLState: my.State}, Message: my.Message}
		}
		return Outcome{Problem: err.Error()}
	}
	// go-mysql's client builds StatusMessage over its pooled read buffer without a copy
	// (utils.ByteSliceToString): the next packet read overwrites it.
	o := Outcome{AffectedRows: res.AffectedRows, InsertID: res.InsertId, Info: strings.Clone(res.StatusMessage)}
	if res.Resultset != nil {
		for _, f := range res.Resultset.Fields {
			o.Columns = append(o.Columns, string(f.Name))
			o.Types = append(o.Types, typeName(f.Type))
		}
		rows, rerr := textRows(res.Resultset, c.Expect.Volatile)
		if rerr != nil {
			return Outcome{Problem: rerr.Error()}
		}
		if !c.Expect.Ordered {
			sort.Strings(rows)
		}
		o.Rows = rows
		if o.Rows == nil {
			o.Rows = []string{}
		}
	}
	return o
}

// diffOutcomes lists what differs between the reference and the system under test;
// error messages are not compared, codes and SQLSTATEs are.
func diffOutcomes(ref, sut Outcome) string {
	var d []string
	add := func(format string, args ...any) { d = append(d, fmt.Sprintf(format, args...)) }
	switch {
	case ref.Problem != "" || sut.Problem != "":
		if ref.Problem != "" {
			add("reference could not run the case: %s", ref.Problem)
		}
		if sut.Problem != "" {
			add("mysqlite could not run the case: %s", sut.Problem)
		}
		return strings.Join(d, "; ")
	case (ref.Error == nil) != (sut.Error == nil):
		add("reference %s, mysqlite %s", describe(ref), describe(sut))
		return strings.Join(d, "; ")
	case ref.Error != nil:
		if *ref.Error != *sut.Error {
			add("error: reference %s, mysqlite %s", describe(ref), describe(sut))
		}
		return strings.Join(d, "; ")
	}
	if !equal(ref.Columns, sut.Columns) {
		add("columns: reference %v, mysqlite %v", ref.Columns, sut.Columns)
	}
	if !equal(ref.Types, sut.Types) {
		add("types: reference %v, mysqlite %v", ref.Types, sut.Types)
	}
	if (ref.Rows == nil) != (sut.Rows == nil) || !equal(ref.Rows, sut.Rows) {
		add("rows: reference %v, mysqlite %v", ref.Rows, sut.Rows)
	}
	if ref.AffectedRows != sut.AffectedRows {
		add("affected rows: reference %d, mysqlite %d", ref.AffectedRows, sut.AffectedRows)
	}
	if ref.InsertID != sut.InsertID {
		add("insert id: reference %d, mysqlite %d", ref.InsertID, sut.InsertID)
	}
	if ref.Info != sut.Info {
		add("info: reference %q, mysqlite %q", ref.Info, sut.Info)
	}
	return strings.Join(d, "; ")
}

func describe(o Outcome) string {
	if o.Error != nil {
		return fmt.Sprintf("error %d (%s) %q", o.Error.Code, o.Error.SQLState, o.Message)
	}
	if o.Rows != nil {
		return fmt.Sprintf("a result set of %d rows", len(o.Rows))
	}
	return fmt.Sprintf("OK (%d affected rows)", o.AffectedRows)
}

// Harness statuses beyond Run's: a case mysqlite refuses with 1235 where the reference
// succeeds is unsupported, a recorded gap rather than wrong data.
const Unsupported Status = "unsupported"

// HarnessResult is one case's outcome on both engines. Oracle explains how the case's
// own expectation differs from the reference (a wrong hypothesis to fix).
type HarnessResult struct {
	ID        string  `json:"id"`
	File      string  `json:"file"`
	Status    Status  `json:"status"`
	Detail    string  `json:"detail,omitempty"`
	Oracle    string  `json:"oracle,omitempty"`
	Deviation string  `json:"deviation,omitempty"`
	Reference Outcome `json:"reference"`
	Mysqlite  Outcome `json:"mysqlite"`
}

// HarnessReport counts the outcomes (section 4: passed / failed / deviated /
// unsupported) and the wrong expectations.
type HarnessReport struct {
	Results     []HarnessResult `json:"results"`
	Passed      int             `json:"passed"`
	Failed      int             `json:"failed"`
	Deviated    int             `json:"deviated"`
	Unsupported int             `json:"unsupported"`
	Oracle      int             `json:"wrong_expectations"`
}

// Summary is the human line of the report.
func (r *HarnessReport) Summary() string {
	return fmt.Sprintf("%d cases: %d passed, %d failed, %d deviated, %d unsupported; %d expectations disagree with the reference",
		len(r.Results), r.Passed, r.Failed, r.Deviated, r.Unsupported, r.Oracle)
}

// OK reports whether the run has no failure: no failed or unsupported case and no
// expectation the reference contradicts. Deviated cases do not fail the run; each one
// is a debt a human approved (section 3).
func (r *HarnessReport) OK() bool {
	return r.Failed == 0 && r.Unsupported == 0 && r.Oracle == 0
}

// Harness runs every case on ref and on sut and compares the outcomes.
func Harness(ctx context.Context, ref, sut Target, cases []Case) *HarnessReport {
	rep := &HarnessReport{}
	for i := range cases {
		c := &cases[i]
		// Each result is read before the next statement runs: the client library reuses
		// its buffers (see outcome).
		refRes, refErr, refProblem := execute(ctx, ref, c)
		refOut := outcome(c, refRes, refErr, refProblem)
		oracle := ""
		if refProblem == "" {
			oracle = compare(&c.Expect, refRes, refErr)
		}
		sutRes, sutErr, sutProblem := execute(ctx, sut, c)
		sutOut := outcome(c, sutRes, sutErr, sutProblem)

		r := HarnessResult{ID: c.ID, File: c.File, Reference: refOut, Mysqlite: sutOut,
			Detail: diffOutcomes(refOut, sutOut), Oracle: oracle}
		if r.Oracle != "" {
			rep.Oracle++
		}
		if c.KnownDeviation != nil {
			r.Deviation = c.KnownDeviation.Reason + " (" + c.KnownDeviation.Ref + ")"
		}
		r.Status = status(c, refOut, sutOut, r.Detail)
		switch r.Status {
		case Passed:
			rep.Passed++
		case Deviated:
			rep.Deviated++
		case Unsupported:
			rep.Unsupported++
		default:
			rep.Failed++
		}
		rep.Results = append(rep.Results, r)
	}
	return rep
}

// status classifies a case from both outcomes and their difference.
func status(c *Case, ref, sut Outcome, detail string) Status {
	switch {
	case ref.Problem != "" || sut.Problem != "":
		// A side that could not run is a failure even when a deviation is recorded:
		// the deviation covers a different answer, not a crash or a lost connection.
		return Failed
	case c.KnownDeviation != nil:
		return Deviated
	case detail == "":
		return Passed
	case ref.Problem == "" && ref.Error == nil && sut.Error != nil && sut.Error.Code == 1235:
		return Unsupported
	}
	return Failed
}

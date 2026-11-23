package corpus

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Every key the harness compares must make a difference visible (docs/07-testing.md
// section 4: rows, column type codes, affected rows, insert id, errno/SQLSTATE).
func TestDiffOutcomes(t *testing.T) {
	base := Outcome{Columns: []string{"a"}, Types: []string{"LONGLONG"}, Rows: []string{`("1")`}}
	ok := Outcome{AffectedRows: 1, InsertID: 7, Info: "x"}
	err := func(code uint16, state string) Outcome {
		return Outcome{Error: &Error{Code: code, SQLState: state}, Message: "m"}
	}
	for _, tc := range []struct {
		name     string
		ref, sut Outcome
		want     string // substring of the difference; "" for none
	}{
		{"same rows", base, base, ""},
		{"same error, other message", err(1146, "42S02"), Outcome{Error: &Error{Code: 1146, SQLState: "42S02"}, Message: "other"}, ""},
		{"column name", base, Outcome{Columns: []string{"b"}, Types: base.Types, Rows: base.Rows}, "columns"},
		{"type code", base, Outcome{Columns: base.Columns, Types: []string{"NEWDECIMAL"}, Rows: base.Rows}, "types"},
		{"row value", base, Outcome{Columns: base.Columns, Types: base.Types, Rows: []string{`("2")`}}, "rows"},
		{"NULL is not the string NULL", base, Outcome{Columns: base.Columns, Types: base.Types, Rows: []string{`(NULL)`}}, "rows"},
		{"no rows against an empty result", Outcome{Rows: []string{}}, ok, "rows"},
		{"affected rows", ok, Outcome{AffectedRows: 2, InsertID: 7, Info: "x"}, "affected rows"},
		{"insert id", ok, Outcome{AffectedRows: 1, InsertID: 8, Info: "x"}, "insert id"},
		{"info", ok, Outcome{AffectedRows: 1, InsertID: 7, Info: "y"}, "info"},
		{"error code", err(1146, "42S02"), err(1054, "42S22"), "error"},
		{"sqlstate", err(1146, "42S02"), err(1146, "HY000"), "error"},
		{"error against success", err(1062, "23000"), ok, "reference error 1062"},
		{"success against error", ok, err(1235, "42000"), "mysqlite error 1235"},
		{"reference cannot run", Outcome{Problem: "setup"}, ok, "reference could not run"},
	} {
		got := diffOutcomes(tc.ref, tc.sut)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: diff %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHarnessStatus(t *testing.T) {
	plain := &Case{}
	deviated := &Case{KnownDeviation: &Deviation{Reason: "r", Ref: "ref"}}
	ok := Outcome{AffectedRows: 1}
	refused := Outcome{Error: &Error{Code: 1235, SQLState: "42000"}}
	wrong := Outcome{Error: &Error{Code: 1105, SQLState: "HY000"}}
	refErr := Outcome{Error: &Error{Code: 1146, SQLState: "42S02"}}
	for _, tc := range []struct {
		name     string
		c        *Case
		ref, sut Outcome
		want     Status
	}{
		{"same", plain, ok, ok, Passed},
		{"1235 where the reference succeeds", plain, ok, refused, Unsupported},
		{"1235 where the reference fails otherwise", plain, refErr, refused, Failed},
		{"another error", plain, ok, wrong, Failed},
		{"recorded deviation", deviated, ok, refused, Deviated},
		// A deviation covers a different answer, never a side that could not run.
		{"deviation, mysqlite cannot run", deviated, ok, Outcome{Problem: "connection lost"}, Failed},
		{"deviation, reference cannot run", deviated, Outcome{Problem: "setup"}, ok, Failed},
	} {
		if got := status(tc.c, tc.ref, tc.sut, diffOutcomes(tc.ref, tc.sut)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	rep := &HarnessReport{Deviated: 3, Passed: 1}
	if !rep.OK() {
		t.Error("deviations alone fail the run")
	}
	for _, r := range []*HarnessReport{{Failed: 1}, {Unsupported: 1}, {Oracle: 1}} {
		if r.OK() {
			t.Errorf("%+v passes", *r)
		}
	}
}

// The harness end to end over the network, with mysqlite on both sides: every case
// runs through CREATE DATABASE and the protocol, the two sides agree, and the only
// expectations the "reference" contradicts are the recorded deviations. With mysql-ref
// as the reference this is M6 criterion 2 (docker).
func TestHarnessSelf(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "test", "corpus"), "")
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := RunPrefix()
	if err != nil {
		t.Fatal(err)
	}
	ref := NewNet(startMysqlite(t).addr, corpusUser, corpusPassword, prefix)
	sut := NewNet(startMysqlite(t).addr, corpusUser, corpusPassword, prefix)
	rep := Harness(context.Background(), ref, sut, cases)
	for _, r := range rep.Results {
		if r.Status != Passed && r.Status != Deviated {
			t.Errorf("%s (%s): %s: %s", r.ID, r.File, r.Status, r.Detail)
		}
		if r.Oracle != "" && r.Status != Deviated {
			t.Errorf("%s: expectation differs from mysqlite: %s", r.ID, r.Oracle)
		}
	}
	if len(rep.Results) != len(cases) || rep.Passed+rep.Deviated != len(cases) {
		t.Errorf("counts: %s", rep.Summary())
	}
	t.Log(rep.Summary())
}

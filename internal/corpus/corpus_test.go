package corpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCorpus(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadFiltersByMilestoneAndKeepsScalarText(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.yaml": `
cases:
  - id: crud.select.002
    milestone: m1
    oracle: pending
    sql: SELECT price FROM t
    expect:
      rows: [[1.50], [null], ["NULL"], [007]]
  - id: crud.select.001
    milestone: m2
    sql: SELECT 1
    expect: {rows: [["1"]]}
`,
		"notes.txt": "ignored",
	})
	cases, err := Load(dir, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].ID != "crud.select.002" || cases[0].File != "a.yaml" {
		t.Fatalf("want the m1 case only, got %+v", cases)
	}
	// Unquoted scalars keep their text: 1.50 is not the float 1.5, 007 not 7.
	got := render(*cases[0].Expect.Rows)
	want := []string{`("1.50")`, `(NULL)`, `("NULL")`, `("007")`}
	if !equal(got, want) {
		t.Fatalf("rows: want %v, got %v", want, got)
	}
	all, err := Load(dir, "")
	if err != nil || len(all) != 2 || all[0].ID != "crud.select.001" {
		t.Fatalf("all milestones sorted by id: %v %+v", err, all)
	}
}

func TestLoadRejectsInvalidCases(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "cases:\n  - {id: a.b.001, milestone: m1, sql: SELECT 1, expect: {rows: []}, colour: red}\n",
		"bad id":           "cases:\n  - {id: nodots, milestone: m1, sql: SELECT 1, expect: {rows: []}}\n",
		"no milestone":     "cases:\n  - {id: a.b.001, sql: SELECT 1, expect: {rows: []}}\n",
		"no sql":           "cases:\n  - {id: a.b.001, milestone: m1, expect: {rows: []}}\n",
		"empty expect":     "cases:\n  - {id: a.b.001, milestone: m1, sql: SELECT 1}\n",
		"error and rows":   "cases:\n  - {id: a.b.001, milestone: m1, sql: SELECT 1, expect: {rows: [], error: {code: 1, sqlstate: HY000}}}\n",
		"bad sqlstate":     "cases:\n  - {id: a.b.001, milestone: m1, sql: SELECT 1, expect: {error: {code: 1064, sqlstate: '42'}}}\n",
		"rows and ok":      "cases:\n  - {id: a.b.001, milestone: m1, sql: SELECT 1, expect: {rows: [], affected_rows: 1}}\n",
		"unknown type":     "cases:\n  - {id: a.b.001, milestone: m1, sql: SELECT 1, expect: {types: [INTEGER]}}\n",
		"bad oracle":       "cases:\n  - {id: a.b.001, milestone: m1, oracle: yes, sql: SELECT 1, expect: {rows: []}}\n",
		"deviation no ref": "cases:\n  - {id: a.b.001, milestone: m1, sql: SELECT 1, expect: {rows: []}, known_deviation: {reason: x}}\n",
		"duplicate id":     "cases:\n  - {id: a.b.001, milestone: m1, sql: SELECT 1, expect: {rows: []}}\n  - {id: a.b.001, milestone: m1, sql: SELECT 2, expect: {rows: []}}\n",
	}
	for name, body := range cases {
		dir := writeCorpus(t, map[string]string{"c.yaml": body})
		if _, err := Load(dir, ""); err == nil {
			t.Errorf("%s: want an error", name)
		} else if !strings.Contains(err.Error(), "c.yaml") {
			t.Errorf("%s: the error names the file: %v", name, err)
		}
	}
}

func TestSQLiteFixture(t *testing.T) {
	got := SQLiteFixture("CREATE TABLE t (id INTEGER PRIMARY KEY auto_increment, note TEXT)")
	if got != "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, note TEXT)" {
		t.Fatalf("got %q", got)
	}
}

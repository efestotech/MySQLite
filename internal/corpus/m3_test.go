package corpus

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"mysqlite/internal/dialect"
)

// TestCorpusM3 runs corpus subset m3 (rule cases and the traps of docs/05-dialect.md
// sections 3, 8, 9, 10) against mysqlite (M3 criteria 1 and 2).
func TestCorpusM3(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "test", "corpus"), "m3")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("corpus subset m3 is empty")
	}
	rep := Run(context.Background(), startMysqlite(t), cases)
	for _, r := range rep.Results {
		switch r.Status {
		case Failed:
			t.Errorf("%s (%s): %s", r.Case.ID, r.Case.File, r.Detail)
		case Deviated:
			t.Logf("%s deviated: %s (%s)", r.Case.ID, r.Case.KnownDeviation.Reason, r.Case.KnownDeviation.Ref)
		}
	}
	t.Log(rep.Summary())
}

// TestRuleCoverage: every rule ID of the registry has a positive and a negative corpus
// case (docs/07-testing.md section 5 item 2), and no case names an unknown rule.
func TestRuleCoverage(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "test", "corpus"), "")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, r := range dialect.Rules() {
		known[r.ID] = true
	}
	type cover struct{ positive, negative bool }
	got := map[string]*cover{}
	for _, c := range cases {
		for _, tag := range c.Tags {
			if len(tag) != 4 || tag[0] != 'R' {
				continue
			}
			if !known[tag] {
				t.Errorf("%s (%s) names unknown rule %s", c.ID, c.File, tag)
				continue
			}
			if got[tag] == nil {
				got[tag] = &cover{}
			}
			got[tag].positive = got[tag].positive || slices.Contains(c.Tags, "positive")
			got[tag].negative = got[tag].negative || slices.Contains(c.Tags, "negative")
		}
	}
	for _, r := range dialect.Rules() {
		c := got[r.ID]
		switch {
		case c == nil || !c.positive:
			t.Errorf("rule %s (%s) has no positive corpus case", r.ID, r.Name)
		case !c.negative:
			t.Errorf("rule %s (%s) has no negative corpus case", r.ID, r.Name)
		}
	}
}

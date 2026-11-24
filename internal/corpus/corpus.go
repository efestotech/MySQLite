// Package corpus loads the differential corpus (docs/07-testing.md section 3) and runs
// it against a MySQL-protocol server. Until the reference engine exists (ADR 0002
// addendum), expectations come from the MySQL documentation and are marked
// `oracle: pending`; a pass then means mysqlite matches the hypothesis, not MySQL.
package corpus

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"go.yaml.in/yaml/v3"
)

// File is one corpus file: one topic.
type File struct {
	// Fixtures holds named setups that cases reuse through YAML anchors.
	Fixtures map[string][]string `yaml:"fixtures"`
	Cases    []Case              `yaml:"cases"`
}

// Case is one corpus case.
type Case struct {
	ID        string   `yaml:"id"`
	Tags      []string `yaml:"tags"`
	Milestone string   `yaml:"milestone"`
	// Oracle is "pending" while the expectation has not been checked against mysql-ref.
	Oracle string `yaml:"oracle"`
	// Setup is the fixture: MySQL statements run on a fresh database before the case.
	Setup []string `yaml:"setup"`
	// Session statements run on the case's connection before SQL and must succeed
	// (for example BEGIN). An extension of section 3 for session state.
	Session []string `yaml:"session"`
	// SQL is the statement under test. In SQL and Session, {{db}} stands for the
	// case's database.
	SQL string `yaml:"sql"`
	// FoundRows makes the client send CLIENT_FOUND_ROWS.
	FoundRows      bool       `yaml:"found_rows"`
	Expect         Expect     `yaml:"expect"`
	KnownDeviation *Deviation `yaml:"known_deviation"`

	// File is the corpus file the case was loaded from.
	File string `yaml:"-"`
}

// Expect is what the statement must produce. Only the keys present are compared.
type Expect struct {
	// Columns are the result column names.
	Columns []string `yaml:"columns"`
	// Types are the column type names without the MYSQL_TYPE_ prefix, e.g. LONGLONG.
	Types []string `yaml:"types"`
	// Rows are the values as the text protocol sends them; null is SQL NULL.
	Rows *[][]*string `yaml:"rows"`
	// Volatile names columns whose values depend on the engine's storage or the moment
	// of the run (sizes, creation times): their values are not compared, their names
	// and types are. In rows, such a value is written "*".
	Volatile []string `yaml:"volatile"`
	// Ordered compares rows in order; otherwise both sides are sorted.
	Ordered      bool    `yaml:"ordered"`
	Error        *Error  `yaml:"error"`
	AffectedRows *uint64 `yaml:"affected_rows"`
	InsertID     *uint64 `yaml:"insert_id"`
	Info         *string `yaml:"info"`
}

// Error is an expected MySQL error.
type Error struct {
	Code     uint16 `yaml:"code"`
	SQLState string `yaml:"sqlstate"`
}

// Deviation is a recorded, visible debt: the case is counted as deviated, never as a
// pass. Adding one needs human approval.
type Deviation struct {
	Reason string `yaml:"reason"`
	Ref    string `yaml:"ref"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9_-]+)+$`)

// Load reads every *.yaml file in dir and returns the cases of milestone (all
// milestones when it is ""), sorted by id. Unknown keys and invalid cases are errors.
func Load(dir, milestone string) ([]Case, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	seen := map[string]string{}
	var cases []Case
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		var f File
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("corpus: %s: %w", p, err)
		}
		for _, c := range f.Cases {
			c.File = filepath.Base(p)
			if err := c.validate(); err != nil {
				return nil, fmt.Errorf("corpus: %s: %w", p, err)
			}
			if other, dup := seen[c.ID]; dup {
				return nil, fmt.Errorf("corpus: %s: case %s already defined in %s", p, c.ID, other)
			}
			seen[c.ID] = c.File
			if milestone == "" || c.Milestone == milestone {
				cases = append(cases, c)
			}
		}
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

func (c *Case) validate() error {
	switch {
	case !idPattern.MatchString(c.ID):
		return fmt.Errorf("case id %q must look like topic.name.001", c.ID)
	case c.Milestone == "":
		return fmt.Errorf("case %s: milestone is required", c.ID)
	case c.SQL == "":
		return fmt.Errorf("case %s: sql is required", c.ID)
	case c.Oracle != "" && c.Oracle != "pending":
		return fmt.Errorf("case %s: oracle must be empty or pending, got %q", c.ID, c.Oracle)
	case c.KnownDeviation != nil && (c.KnownDeviation.Reason == "" || c.KnownDeviation.Ref == ""):
		return fmt.Errorf("case %s: known_deviation needs a reason and a ref", c.ID)
	}
	e := c.Expect
	result := e.Rows != nil || e.Columns != nil || e.Types != nil
	ok := e.AffectedRows != nil || e.InsertID != nil || e.Info != nil
	switch {
	case !result && !ok && e.Error == nil:
		return fmt.Errorf("case %s: expect is empty", c.ID)
	case e.Error != nil && (result || ok):
		return fmt.Errorf("case %s: expect error excludes every other key", c.ID)
	case e.Error != nil && (e.Error.Code == 0 || len(e.Error.SQLState) != 5):
		return fmt.Errorf("case %s: expect error needs a code and a 5-character sqlstate", c.ID)
	case result && ok:
		return fmt.Errorf("case %s: a result set has no affected rows, insert id or info", c.ID)
	}
	for _, name := range e.Types {
		if _, known := typeCodes[name]; !known {
			return fmt.Errorf("case %s: unknown column type %q", c.ID, name)
		}
	}
	return nil
}

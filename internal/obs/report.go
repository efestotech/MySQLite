package obs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Construct is one unsupported construct of the journal: how often clients met its
// 1235 (docs/05-dialect.md section 2, the roadmap generator).
type Construct struct {
	Name string `json:"construct"`
	// Count is the number of statements refused for it.
	Count int `json:"count"`
	// Sessions is the number of distinct connections that met it.
	Sessions int `json:"sessions"`
	// Statements is the number of distinct statement texts refused for it.
	Statements int       `json:"distinct_statements"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	// Example is the first statement refused for it.
	Example string `json:"example"`
}

// Report ranks the unsupported constructs of one or more journal files.
type Report struct {
	// Statements counts the journal entries read.
	Statements int `json:"statements"`
	// Unsupported counts the entries refused with 1235.
	Unsupported int `json:"unsupported"`
	// Malformed counts lines that are not journal entries (a line cut by a crash).
	Malformed int `json:"malformed_lines"`
	// Constructs is ordered by Count, highest first, then by name.
	Constructs []Construct `json:"constructs"`
}

// JournalFiles lists the journal files of a log directory: journal.jsonl and its
// rotated backups (journal-<time>.jsonl), sorted by name.
func JournalFiles(dir string) ([]string, error) {
	var files []string
	for _, pattern := range []string{"journal.jsonl", "journal-*.jsonl"} {
		m, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	return files, nil
}

// ReadReport tallies the unsupported constructs of the journal files.
func ReadReport(files []string) (*Report, error) {
	type tally struct {
		c          Construct
		sessions   map[uint32]bool
		statements map[string]bool
	}
	byName := map[string]*tally{}
	rep := &Report{}
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		err = eachLine(f, func(line []byte) {
			var r record
			if json.Unmarshal(line, &r) != nil || r.SQL == "" && r.Outcome == "" {
				rep.Malformed++
				return
			}
			rep.Statements++
			if r.Unsupported == "" {
				return
			}
			rep.Unsupported++
			ts, _ := time.Parse(time.RFC3339Nano, r.Time)
			t := byName[r.Unsupported]
			if t == nil {
				t = &tally{c: Construct{Name: r.Unsupported, Example: r.SQL, First: ts, Last: ts},
					sessions: map[uint32]bool{}, statements: map[string]bool{}}
				byName[r.Unsupported] = t
			}
			t.c.Count++
			t.sessions[r.Session] = true
			t.statements[r.SQL] = true
			if ts.Before(t.c.First) {
				t.c.First = ts
			}
			if ts.After(t.c.Last) {
				t.c.Last = ts
			}
		})
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, t := range byName {
		t.c.Sessions, t.c.Statements = len(t.sessions), len(t.statements)
		rep.Constructs = append(rep.Constructs, t.c)
	}
	sort.Slice(rep.Constructs, func(i, j int) bool {
		a, b := rep.Constructs[i], rep.Constructs[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Name < b.Name
	})
	return rep, nil
}

// eachLine calls fn for every non-empty line of r. Lines have no length limit: a
// journalled statement can be as long as max_allowed_packet.
func eachLine(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if trimmed := trimEOL(line); len(trimmed) > 0 {
				fn(trimmed)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

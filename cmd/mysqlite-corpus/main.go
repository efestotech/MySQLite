// Command mysqlite-corpus works on the query journal (docs/07-testing.md section 5).
//
//	mysqlite-corpus report [-json] [-top N] PATH...
//
// report ranks the constructs mysqlite refused with 1235 by how often clients sent
// them, the roadmap of docs/05-dialect.md section 2. A PATH is a journal file or a log
// directory, whose journal.jsonl and rotated journal-*.jsonl are all read.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"mysqlite/internal/obs"
)

const usage = `usage: mysqlite-corpus report [-json] [-top N] PATH...

report  rank the unsupported constructs (1235) of the query journal by frequency.
        PATH is a journal file or a log directory (journal.jsonl and its rotated
        journal-*.jsonl files).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes a command and returns the exit code: 0 done, 1 failed, 2 bad usage.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "report" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	top := fs.Int("top", 0, "print only the N most frequent constructs (0: all)")
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() == 0 || *top < 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	files, err := journalFiles(fs.Args())
	if err != nil {
		fmt.Fprintln(stderr, "mysqlite-corpus:", err)
		return 1
	}
	rep, err := obs.ReadReport(files)
	if err != nil {
		fmt.Fprintln(stderr, "mysqlite-corpus:", err)
		return 1
	}
	if *top > 0 && len(rep.Constructs) > *top {
		rep.Constructs = rep.Constructs[:*top]
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "mysqlite-corpus:", err)
			return 1
		}
		return 0
	}
	printReport(stdout, rep)
	if rep.Malformed > 0 {
		fmt.Fprintf(stderr, "mysqlite-corpus: skipped %d malformed journal lines\n", rep.Malformed)
	}
	return 0
}

// journalFiles expands the PATH arguments; a directory without journal files is an
// error, so a mistyped path does not read as an empty journal.
func journalFiles(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, p)
			continue
		}
		found, err := obs.JournalFiles(p)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("%s: no journal.jsonl or journal-*.jsonl", p)
		}
		files = append(files, found...)
	}
	return files, nil
}

func printReport(w io.Writer, rep *obs.Report) {
	share := 0.0
	if rep.Statements > 0 {
		share = 100 * float64(rep.Unsupported) / float64(rep.Statements)
	}
	fmt.Fprintf(w, "%d statements, %d unsupported (%.1f%%)\n", rep.Statements, rep.Unsupported, share)
	if len(rep.Constructs) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nRANK\tCOUNT\tSESSIONS\tSTATEMENTS\tCONSTRUCT\tEXAMPLE")
	for i, c := range rep.Constructs {
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%s\t%s\n", i+1, c.Count, c.Sessions, c.Statements, c.Name, oneLine(c.Example, 80))
	}
	tw.Flush()
}

// oneLine collapses whitespace runs to one space and cuts s to n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}

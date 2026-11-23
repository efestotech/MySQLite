package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mysqlite/internal/corpus"
)

// dsn is user:password@host:port.
type dsn struct{ user, password, addr string }

func parseDSN(s string) (dsn, error) {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return dsn{}, fmt.Errorf("%q: want user:password@host:port", s)
	}
	user, password, _ := strings.Cut(s[:at], ":")
	addr := s[at+1:]
	if user == "" || !strings.Contains(addr, ":") {
		return dsn{}, fmt.Errorf("%q: want user:password@host:port", s)
	}
	return dsn{user, password, addr}, nil
}

func harness(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	fs.SetOutput(stderr)
	refFlag := fs.String("ref", "", "reference engine, user:password@host:port")
	sutFlag := fs.String("sut", "", "mysqlite, user:password@host:port")
	milestone := fs.String("milestone", "", "run only the cases of this milestone")
	jsonOut := fs.String("json", "", "write the full report to this file")
	golden := fs.String("golden", "", "write the reference outcome of each case to this directory")
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *refFlag == "" || *sutFlag == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	ref, err := parseDSN(*refFlag)
	if err == nil {
		var sut dsn
		if sut, err = parseDSN(*sutFlag); err == nil {
			return runHarness(ref, sut, fs.Arg(0), *milestone, *jsonOut, *golden, stdout, stderr)
		}
	}
	fmt.Fprintln(stderr, "mysqlite-corpus:", err)
	return 2
}

func runHarness(ref, sut dsn, dir, milestone, jsonOut, golden string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "mysqlite-corpus:", err)
		return 1
	}
	cases, err := corpus.Load(dir, milestone)
	if err != nil {
		return fail(err)
	}
	if len(cases) == 0 {
		return fail(errors.New("no corpus case selected"))
	}
	prefix, err := corpus.RunPrefix()
	if err != nil {
		return fail(err)
	}
	refT := corpus.NewNet(ref.addr, ref.user, ref.password, prefix)
	sutT := corpus.NewNet(sut.addr, sut.user, sut.password, prefix)
	for _, t := range []*corpus.Net{refT, sutT} {
		if err := t.Wait(60 * time.Second); err != nil {
			return fail(err)
		}
	}
	rep := corpus.Harness(context.Background(), refT, sutT, cases)
	// mysql-ref is cleaned; mysqlite has no DROP DATABASE yet, so its case databases
	// stay in its data directory (each run has its own prefix).
	if err := refT.DropAll(); err != nil {
		fmt.Fprintln(stderr, "mysqlite-corpus: cleaning the reference:", err)
	}

	for _, r := range rep.Results {
		switch {
		case r.Status == corpus.Failed || r.Status == corpus.Unsupported:
			fmt.Fprintf(stdout, "%s %s (%s): %s\n", strings.ToUpper(string(r.Status)), r.ID, r.File, r.Detail)
		case r.Status == corpus.Deviated:
			fmt.Fprintf(stdout, "DEVIATED %s: %s\n", r.ID, r.Deviation)
		}
		if r.Oracle != "" {
			fmt.Fprintf(stdout, "EXPECTATION %s (%s): the reference disagrees: %s\n", r.ID, r.File, r.Oracle)
		}
	}
	fmt.Fprintln(stdout, rep.Summary())

	if jsonOut != "" {
		if err := writeJSON(jsonOut, rep); err != nil {
			return fail(err)
		}
	}
	if golden != "" {
		for _, r := range rep.Results {
			if err := writeJSON(filepath.Join(golden, r.ID+".json"), r.Reference); err != nil {
				return fail(err)
			}
		}
	}
	if !rep.OK() {
		return 1
	}
	return 0
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

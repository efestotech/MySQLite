// Command notice audits the licenses of every module linked into the mysqlite server and
// writes the NOTICE file (docs/02-research.md section 6).
//
//	go run ./scripts/notice -check        # fail on a forbidden or unknown license
//	go run ./scripts/notice -o NOTICE     # regenerate NOTICE
//
// For each linked package it takes the license files of the nearest directory, from the
// package up to its module root, that holds one: the same rule go-licenses uses. The
// primary file of that directory (LICENSE, LICENCE or COPYING) decides the license; the
// other license-named files there (NOTICE, third-party lists) are carried into NOTICE
// verbatim. CI also runs `go-licenses check` (docs/07-testing.md section 8).
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// rule recognizes one license by a phrase of its text.
type rule struct {
	name    string
	allowed bool
	re      *regexp.Regexp
}

// rules recognize the licenses a primary license file can hold. A file can hold several
// (go-yaml's LICENSE is MIT and Apache-2.0); every one found is reported.
var rules = []rule{
	{"AGPL", false, regexp.MustCompile(`GNU AFFERO GENERAL PUBLIC`)},
	{"LGPL", false, regexp.MustCompile(`GNU (LESSER|LIBRARY) GENERAL PUBLIC`)},
	{"GPL", false, regexp.MustCompile(`GNU GENERAL PUBLIC LICENSE`)},
	{"SSPL", false, regexp.MustCompile(`Server Side Public License`)},
	{"BUSL", false, regexp.MustCompile(`Business Source License`)},
	{"MPL-2.0", false, regexp.MustCompile(`Mozilla Public License,? [Vv]ersion 2\.0`)},
	{"Apache-2.0", true, regexp.MustCompile(`Apache License,?\s+Version 2\.0`)},
	{"MIT", true, regexp.MustCompile(`Permission is hereby granted, free of charge`)},
	{"BSD-3-Clause", true, regexp.MustCompile(`Neither the name|names of its\s+contributors`)},
	{"BSD-2-Clause", true, regexp.MustCompile(`Redistributions in binary form must reproduce`)},
	{"ISC", true, regexp.MustCompile(`Permission to use, copy, modify, and(/or)? distribute`)},
	{"Public-Domain", true, regexp.MustCompile(`dedicated to the public\s+domain`)},
}

// classify names the licenses of a primary license text; allowed is false when any of
// them is forbidden or when no rule recognizes the text.
func classify(text string) (name string, allowed bool) {
	var names []string
	allowed = true
	for _, r := range rules {
		// BSD-3-Clause contains the BSD-2-Clause wording: one text, one name.
		if r.name == "BSD-2-Clause" && len(names) > 0 && names[len(names)-1] == "BSD-3-Clause" {
			continue
		}
		if r.re.MatchString(text) {
			names = append(names, r.name)
			allowed = allowed && r.allowed
		}
	}
	if len(names) == 0 {
		return "unknown", false
	}
	return strings.Join(names, ", "), allowed
}

var primaryName = regexp.MustCompile(`(?i)^(licen[cs]e|copying)(\.(txt|md))?$`)
var licenseName = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice)`)

// pkg is one linked package outside the main module.
type pkg struct{ module, version, dir, moduleDir string }

// component is one license directory and the modules that take their license from it.
type component struct {
	module, version string
	dir             string   // directory holding the license files
	rel             string   // dir relative to the module root, "" for the root
	primary         string   // primary license file name
	license         string   // classified license
	allowed         bool     //
	extra           []string // other license-named files in dir
}

func main() {
	check := flag.Bool("check", false, "fail on a forbidden or unknown license, write nothing")
	out := flag.String("o", "", "write NOTICE to this file (default stdout)")
	flag.Parse()
	comps, err := audit("mysqlite/cmd/mysqlite")
	if err != nil {
		fmt.Fprintln(os.Stderr, "notice:", err)
		os.Exit(1)
	}
	bad := 0
	for _, c := range comps {
		if !c.allowed {
			bad++
			fmt.Fprintf(os.Stderr, "notice: %s %s (%s): license %s is not allowed\n", c.module, c.version, c.rel, c.license)
		}
	}
	if bad > 0 {
		os.Exit(1)
	}
	if *check {
		for _, c := range comps {
			fmt.Printf("%s %s %s: %s\n", c.module, c.version, displayRel(c.rel), c.license)
		}
		return
	}
	var buf bytes.Buffer
	if err := write(&buf, comps); err != nil {
		fmt.Fprintln(os.Stderr, "notice:", err)
		os.Exit(1)
	}
	if *out == "" {
		_, err = os.Stdout.Write(buf.Bytes())
	} else {
		err = os.WriteFile(*out, buf.Bytes(), 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "notice:", err)
		os.Exit(1)
	}
}

func displayRel(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// audit lists the packages linked into target for linux (the release platform) and
// resolves each one's license directory. The Go standard library is added from GOROOT.
func audit(target string) ([]component, error) {
	pkgs, err := linked(target)
	if err != nil {
		return nil, err
	}
	byDir := map[string]*component{}
	for _, p := range pkgs {
		dir, err := licenseDir(p.dir, p.moduleDir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.module, err)
		}
		if byDir[dir] != nil {
			continue
		}
		c, err := describe(p.module, p.version, dir, p.moduleDir)
		if err != nil {
			return nil, err
		}
		byDir[dir] = c
	}
	goroot, err := goOutput("env", "GOROOT")
	if err != nil {
		return nil, err
	}
	goversion, err := goOutput("env", "GOVERSION")
	if err != nil {
		return nil, err
	}
	std, err := describe("Go standard library", goversion, goroot, goroot)
	if err != nil {
		return nil, err
	}
	comps := []component{*std}
	for _, c := range byDir {
		comps = append(comps, *c)
	}
	sort.Slice(comps[1:], func(i, j int) bool {
		a, b := comps[1+i], comps[1+j]
		if a.module != b.module {
			return a.module < b.module
		}
		return a.rel < b.rel
	})
	return comps, nil
}

// linked returns the non-standard packages target links on linux.
func linked(target string) ([]pkg, error) {
	// -buildvcs=false: the audit needs no VCS stamp, and git refuses to report status
	// for a checkout owned by another user (the toolchain container runs as root).
	cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f",
		"{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}|{{.Module.Version}}|{{.Dir}}|{{.Module.Dir}}{{end}}{{end}}", target)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, stderr.String())
	}
	var out []pkg
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if f := strings.Split(sc.Text(), "|"); len(f) == 4 {
			out = append(out, pkg{f[0], f[1], f[2], f[3]})
		}
	}
	return out, sc.Err()
}

func goOutput(args ...string) (string, error) {
	b, err := exec.Command("go", args...).Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(b)), nil
}

// licenseDir is the nearest directory from dir up to root that holds a primary license
// file.
func licenseDir(dir, root string) (string, error) {
	root = filepath.Clean(root)
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if name, err := primaryIn(d); err != nil {
			return "", err
		} else if name != "" {
			return d, nil
		}
		if d == root || filepath.Dir(d) == d || !strings.HasPrefix(d, root) {
			return "", errors.New("no license file up to the module root")
		}
	}
}

func primaryIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() && primaryName.MatchString(e.Name()) {
			return e.Name(), nil
		}
	}
	return "", nil
}

func describe(module, version, dir, root string) (*component, error) {
	primary, err := primaryIn(dir)
	if err != nil {
		return nil, err
	}
	if primary == "" {
		return nil, fmt.Errorf("%s: no license file in %s", module, dir)
	}
	text, err := os.ReadFile(filepath.Join(dir, primary))
	if err != nil {
		return nil, err
	}
	name, allowed := classify(string(text))
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		rel = ""
	}
	c := &component{module: module, version: version, dir: dir, rel: filepath.ToSlash(rel),
		primary: primary, license: name, allowed: allowed}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && e.Name() != primary && licenseName.MatchString(e.Name()) {
			c.extra = append(c.extra, e.Name())
		}
	}
	return c, nil
}

const header = `mysqlite
Copyright 2026 The mysqlite authors

This product is licensed under the Apache License, Version 2.0 (see LICENSE).

The mysqlite server binary includes the third-party software listed below. Each
component's license and notice files are reproduced in full. This file is generated by
"go run ./scripts/notice -o NOTICE"; do not edit it by hand.
`

const rulerLine = "================================================================================"

func write(w io.Writer, comps []component) error {
	fmt.Fprint(w, header)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Components:")
	fmt.Fprintln(w)
	for _, c := range comps {
		fmt.Fprintf(w, "  %s %s%s: %s\n", c.module, c.version, relSuffix(c.rel), c.license)
	}
	for _, c := range comps {
		for _, name := range append([]string{c.primary}, c.extra...) {
			text, err := os.ReadFile(filepath.Join(c.dir, name))
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "\n%s\n%s %s%s: %s\n%s\n\n", rulerLine, c.module, c.version, relSuffix(c.rel), name, rulerLine)
			body := strings.ReplaceAll(string(text), "\r\n", "\n")
			fmt.Fprint(w, strings.TrimRight(body, "\n \t"), "\n")
		}
	}
	return nil
}

func relSuffix(rel string) string {
	if rel == "" {
		return ""
	}
	return " (" + rel + ")"
}

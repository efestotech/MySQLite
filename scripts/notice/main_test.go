package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// Allowed: Apache-2.0, MIT, BSD-2/3, ISC, public domain. Forbidden: GPL, AGPL, LGPL,
// SSPL, BUSL and anything unclear (docs/02-research.md section 6).
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		text    string
		name    string
		allowed bool
	}{
		{"Apache License\n                           Version 2.0, January 2004", "Apache-2.0", true},
		{"Licensed under the Apache License, Version 2.0 (the \"License\")", "Apache-2.0", true},
		{"Permission is hereby granted, free of charge, to any person", "MIT", true},
		{"Redistributions in binary form must reproduce ...\n* Neither the name of Google Inc.", "BSD-3-Clause", true},
		{"Redistributions in binary form must reproduce the above copyright", "BSD-2-Clause", true},
		{"Permission to use, copy, modify, and/or distribute this software", "ISC", true},
		{"has been dedicated to the public\ndomain by the authors", "Public-Domain", true},
		{"GNU GENERAL PUBLIC LICENSE\nVersion 2, June 1991", "GPL", false},
		{"GNU AFFERO GENERAL PUBLIC LICENSE", "AGPL", false},
		{"GNU LESSER GENERAL PUBLIC LICENSE", "LGPL", false},
		{"Server Side Public License", "SSPL", false},
		{"Business Source License 1.1", "BUSL", false},
		{"Mozilla Public License Version 2.0", "MPL-2.0", false},
		{"All rights reserved.", "unknown", false},
		// A permissive text bundled with a forbidden one is forbidden.
		{"Permission is hereby granted, free of charge\nGNU GENERAL PUBLIC LICENSE", "GPL, MIT", false},
	} {
		name, allowed := classify(tc.text)
		if name != tc.name || allowed != tc.allowed {
			t.Errorf("classify(%.40q) = %q, %v; want %q, %v", tc.text, name, allowed, tc.name, tc.allowed)
		}
	}
}

// The committed NOTICE is what the generator writes for the current module graph, and
// every linked license is allowed.
func TestNoticeUpToDate(t *testing.T) {
	comps, err := audit("mysqlite/cmd/mysqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range comps {
		if !c.allowed {
			t.Errorf("%s %s (%s): license %s is not allowed", c.module, c.version, c.rel, c.license)
		}
	}
	var buf bytes.Buffer
	if err := write(&buf, comps); err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(string(committed), "\r\n", "\n")
	if got != buf.String() {
		t.Fatal("NOTICE is out of date: run go run ./scripts/notice -o NOTICE")
	}
}

// Package tz resolves the values of the time_zone variable (ADR 0007): SYSTEM, UTC and
// offsets such as +05:30. Named zones are refused by SET (1298) and never reach it.
package tz

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

var fixed sync.Map // offset text -> *time.Location

// Location is the zone a time_zone value names. SYSTEM is the host's zone, with its
// daylight-saving rules.
func Location(spec string) (*time.Location, error) {
	switch strings.ToUpper(spec) {
	case "SYSTEM", "":
		return time.Local, nil
	case "UTC":
		return time.UTC, nil
	}
	if l, ok := fixed.Load(spec); ok {
		return l.(*time.Location), nil
	}
	if len(spec) < 5 || spec[0] != '+' && spec[0] != '-' {
		return nil, fmt.Errorf("tz: time zone %q", spec)
	}
	h, m, ok := strings.Cut(spec[1:], ":")
	hh, e1 := strconv.Atoi(h)
	mm, e2 := strconv.Atoi(m)
	if !ok || e1 != nil || e2 != nil {
		return nil, fmt.Errorf("tz: time zone %q", spec)
	}
	secs := (hh*60 + mm) * 60
	if spec[0] == '-' {
		secs = -secs
	}
	l := time.FixedZone(spec, secs)
	fixed.Store(spec, l)
	return l, nil
}

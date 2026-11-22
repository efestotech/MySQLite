package udf

import (
	"strings"
	"time"

	"mysqlite/internal/tz"
)

// TIMESTAMP columns hold UTC (ADR 0007). tzFromUTC(v, zone) shows a stored value in the
// session zone; tzToUTC(v, zone) converts a value written in the session zone for
// storage. Values that are not a date and time (NULL, the zero date, a number) pass
// unchanged, as do zones tz cannot read.

func tzFromUTC(args []any) (any, error) {
	return convertZone(args, true), nil
}

func tzToUTC(args []any) (any, error) {
	return convertZone(args, false), nil
}

const datetimeLayout = "2006-01-02 15:04:05"

func convertZone(args []any, fromUTC bool) any {
	s, ok := textArg(args[0])
	if !ok {
		return args[0]
	}
	zone, _ := textArg(args[1])
	loc, err := tz.Location(zone)
	if err != nil {
		return args[0]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if len(whole) == len("2006-01-02") {
		whole += " 00:00:00"
	}
	if len(whole) != len(datetimeLayout) || strings.HasPrefix(whole, "0000-00-00") {
		return args[0]
	}
	src, dst := loc, time.UTC
	if fromUTC {
		src, dst = time.UTC, loc
	}
	t, err := time.ParseInLocation(datetimeLayout, whole, src)
	if err != nil {
		return args[0]
	}
	out := t.In(dst).Format(datetimeLayout)
	if hasFrac {
		out += "." + frac
	}
	return out
}

func textArg(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	}
	return "", false
}

package udf

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Date arithmetic (docs/05-dialect.md section 5). Units are MySQL's INTERVAL units,
// passed as text by the dialect.

// unitParts are the fields of each composite unit, largest first.
var unitParts = map[string][]string{
	"YEAR_MONTH":         {"YEAR", "MONTH"},
	"DAY_HOUR":           {"DAY", "HOUR"},
	"DAY_MINUTE":         {"DAY", "HOUR", "MINUTE"},
	"DAY_SECOND":         {"DAY", "HOUR", "MINUTE", "SECOND"},
	"DAY_MICROSECOND":    {"DAY", "HOUR", "MINUTE", "SECOND", "MICROSECOND"},
	"HOUR_MINUTE":        {"HOUR", "MINUTE"},
	"HOUR_SECOND":        {"HOUR", "MINUTE", "SECOND"},
	"HOUR_MICROSECOND":   {"HOUR", "MINUTE", "SECOND", "MICROSECOND"},
	"MINUTE_SECOND":      {"MINUTE", "SECOND"},
	"MINUTE_MICROSECOND": {"MINUTE", "SECOND", "MICROSECOND"},
	"SECOND_MICROSECOND": {"SECOND", "MICROSECOND"},
}

// interval is an amount of months and microseconds.
type interval struct {
	months int64
	micros int64
}

var unitMicros = map[string]int64{
	"MICROSECOND": 1,
	"SECOND":      1e6,
	"MINUTE":      60e6,
	"HOUR":        3600e6,
	"DAY":         86400e6,
	"WEEK":        7 * 86400e6,
}

// parseInterval reads INTERVAL v unit. ok is false for a value MySQL rejects (NULL).
func parseInterval(v any, unit string) (interval, bool) {
	if v == nil {
		return interval{}, false
	}
	add := func(iv *interval, part string, n int64) {
		switch part {
		case "YEAR":
			iv.months += n * 12
		case "QUARTER":
			iv.months += n * 3
		case "MONTH":
			iv.months += n
		default:
			iv.micros += n * unitMicros[part]
		}
	}
	if parts, composite := unitParts[unit]; composite {
		s, _ := textArg(v)
		if s == "" {
			s = fmt.Sprint(v)
		}
		s = strings.TrimSpace(s)
		neg := strings.HasPrefix(s, "-")
		nums := strings.FieldsFunc(strings.TrimPrefix(s, "-"), func(r rune) bool { return r < '0' || r > '9' })
		if len(nums) == 0 || len(nums) > len(parts) {
			return interval{}, false
		}
		var iv interval
		// Values align to the right: '30' HOUR_MINUTE is 30 minutes.
		offset := len(parts) - len(nums)
		for i, txt := range nums {
			n, err := strconv.ParseInt(txt, 10, 64)
			if err != nil {
				return interval{}, false
			}
			part := parts[offset+i]
			if part == "MICROSECOND" {
				n *= int64(math.Pow10(6 - min(len(txt), 6)))
			}
			add(&iv, part, n)
		}
		if neg {
			iv.months, iv.micros = -iv.months, -iv.micros
		}
		return iv, true
	}
	var f float64
	switch x := v.(type) {
	case int64:
		f = float64(x)
	case float64:
		f = x
	default:
		s, _ := textArg(v)
		f = toFloat(parseNumber(s))
	}
	var iv interval
	if unit == "SECOND" {
		iv.micros = int64(math.Round(f * 1e6))
		return iv, true
	}
	n := int64(math.Round(f))
	if _, ok := unitMicros[unit]; !ok && unit != "YEAR" && unit != "QUARTER" && unit != "MONTH" {
		return interval{}, false
	}
	add(&iv, unit, n)
	return iv, true
}

func isDateUnit(unit string) bool {
	switch unit {
	case "YEAR", "QUARTER", "MONTH", "WEEK", "DAY", "YEAR_MONTH":
		return true
	}
	return false
}

// dateAdd(d, v, unit, sign) is DATE_ADD (sign 1) and DATE_SUB (sign -1). A date with
// a date unit stays a date; months clamp the day to the month's last; a result outside
// years 0-9999 is NULL.
func dateAdd(args []any) (any, error) {
	unit, _ := textArg(args[2])
	sign, _ := args[3].(int64)
	t, ok := parseTemporal(args[0], false)
	if !ok {
		return nil, nil
	}
	iv, ok := parseInterval(args[1], unit)
	if !ok {
		return nil, nil
	}
	iv.months *= sign
	iv.micros *= sign
	r, ok := addInterval(t, iv)
	if !ok {
		return nil, nil
	}
	if !t.hasClock && isDateUnit(unit) {
		return fmt.Sprintf("%04d-%02d-%02d", r.year, r.month, r.day), nil
	}
	out := fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", r.year, r.month, r.day, r.hour, r.minute, r.second)
	if r.micro != 0 {
		out += fmt.Sprintf(".%06d", r.micro)
	}
	return out, nil
}

func addInterval(t temporal, iv interval) (temporal, bool) {
	if iv.months != 0 {
		m := int64(t.year)*12 + int64(t.month-1) + iv.months
		if m < 0 || m >= 10000*12 {
			return temporal{}, false
		}
		t.year, t.month = int(m/12), int(m%12)+1
		t.day = min(t.day, daysIn(t.year, t.month))
	}
	if iv.micros != 0 {
		tt := t.time().Add(time.Duration(iv.micros) * time.Microsecond)
		if tt.Year() < 0 || tt.Year() > 9999 {
			return temporal{}, false
		}
		t.year, t.month, t.day = tt.Year(), int(tt.Month()), tt.Day()
		t.hour, t.minute, t.second, t.micro = tt.Hour(), tt.Minute(), tt.Second(), tt.Nanosecond()/1000
	}
	return t, true
}

// timestampDiff(unit, a, b) is TIMESTAMPDIFF: b - a in whole units, toward zero.
func timestampDiff(args []any) (any, error) {
	unit, _ := textArg(args[0])
	a, ok1 := parseTemporal(args[1], false)
	b, ok2 := parseTemporal(args[2], false)
	if !ok1 || !ok2 {
		return nil, nil
	}
	switch unit {
	case "YEAR", "QUARTER", "MONTH":
		months := monthsBetween(a, b)
		div := map[string]int64{"YEAR": 12, "QUARTER": 3, "MONTH": 1}[unit]
		return months / div, nil
	}
	per, ok := unitMicros[unit]
	if !ok {
		return nil, fmt.Errorf("udf: TIMESTAMPDIFF unit %q", unit)
	}
	return int64(b.time().Sub(a.time())/time.Microsecond) / per, nil
}

// monthsBetween counts the whole months from a to b: a month is complete when b's day
// and time of day reach a's.
func monthsBetween(a, b temporal) int64 {
	months := int64(b.year-a.year)*12 + int64(b.month-a.month)
	rest := func(t temporal) int64 {
		return ((int64(t.day)*24+int64(t.hour))*60+int64(t.minute))*60e6 + int64(t.second)*1e6 + int64(t.micro)
	}
	switch {
	case months > 0 && rest(b) < rest(a):
		months--
	case months < 0 && rest(b) > rest(a):
		months++
	}
	return months
}

// extract(unit, d) is EXTRACT(unit FROM d); composite units concatenate their fields.
func extract(args []any) (any, error) {
	unit, _ := textArg(args[0])
	t, ok := parseTemporal(args[1], true)
	if !ok {
		return nil, nil
	}
	field := func(part string) int64 {
		switch part {
		case "YEAR":
			return int64(t.year)
		case "MONTH":
			return int64(t.month)
		case "DAY":
			return int64(t.day)
		case "HOUR":
			return int64(t.hour)
		case "MINUTE":
			return int64(t.minute)
		case "SECOND":
			return int64(t.second)
		case "MICROSECOND":
			return int64(t.micro)
		case "QUARTER":
			return int64((t.month + 2) / 3)
		case "WEEK":
			w, _ := weekMode(t.time(), 0)
			return int64(w)
		}
		return 0
	}
	parts, composite := unitParts[unit]
	if !composite {
		return field(unit), nil
	}
	var n int64
	for _, p := range parts {
		scale := int64(100)
		if p == "MICROSECOND" {
			scale = 1e6
		}
		n = n*scale + field(p)
	}
	return n, nil
}

// weekOf(d, mode) is WEEK(d, mode) for modes 0-7.
func weekOf(args []any) (any, error) {
	t, ok := parseTemporal(args[0], false)
	if !ok || args[1] == nil {
		return nil, nil
	}
	mode, _ := args[1].(int64)
	w, _ := weekMode(t.time(), int(mode&7))
	return int64(w), nil
}

// weekMode numbers a date by MySQL's WEEK modes, from their documented definitions:
// the first day of the week (Sunday for even modes but 0-3 swap: see weekRules), the
// range (0-53 or 1-53) and what makes week 1 (the first week with 4 or more days in
// the year, or the week holding the first such weekday). It returns the year the
// week belongs to.
func weekMode(t time.Time, mode int) (int, int) {
	r := weekRules[mode]
	start := func(year int) time.Time {
		jan1 := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		wd := (int(jan1.Weekday()) - int(r.first) + 7) % 7
		if r.fourDays && wd <= 3 || wd == 0 {
			return jan1.AddDate(0, 0, -wd)
		}
		return jan1.AddDate(0, 0, 7-wd)
	}
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	year := t.Year()
	if r.oneBased && r.fourDays && !day.Before(start(year+1)) {
		return 1, year + 1
	}
	s := start(year)
	if day.Before(s) {
		if !r.oneBased {
			return 0, year
		}
		year--
		s = start(year)
	}
	return int(day.Sub(s).Hours()/24)/7 + 1, year
}

type weekRule struct {
	first    time.Weekday
	oneBased bool
	fourDays bool
}

var weekRules = [8]weekRule{
	{time.Sunday, false, false},
	{time.Monday, false, true},
	{time.Sunday, true, false},
	{time.Monday, true, true},
	{time.Sunday, false, true},
	{time.Monday, false, false},
	{time.Sunday, true, true},
	{time.Monday, true, false},
}

// timeOf(d) is TIME(d): the time of day, keeping the fraction written.
func timeOf(args []any) (any, error) {
	t, ok := parseTemporal(args[0], true)
	if !ok || !t.hasClock {
		if ok && t.hasDate {
			return "00:00:00", nil
		}
		return nil, nil
	}
	out := fmt.Sprintf("%02d:%02d:%02d", t.hour, t.minute, t.second)
	if t.micro != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%06d", t.micro), "0")
	}
	return out, nil
}

// makeDate(year, dayofyear) is MAKEDATE; two-digit years as MySQL reads them.
func makeDate(args []any) (any, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	y := int(toFloat(numberArg(args[0])))
	d := int(toFloat(numberArg(args[1])))
	if d <= 0 || y < 0 {
		return nil, nil
	}
	switch {
	case y < 70:
		y += 2000
	case y < 100:
		y += 1900
	}
	t := time.Date(y, 1, d, 0, 0, 0, 0, time.UTC)
	if t.Year() > 9999 {
		return nil, nil
	}
	return t.Format("2006-01-02"), nil
}

// periodDiff(p1, p2) is PERIOD_DIFF: months between YYMM or YYYYMM periods.
func periodDiff(args []any) (any, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	months := func(p int64) int64 {
		y, m := p/100, p%100
		switch {
		case y < 70:
			y += 2000
		case y < 100:
			y += 1900
		}
		return y*12 + m - 1
	}
	return months(int64(toFloat(numberArg(args[0])))) - months(int64(toFloat(numberArg(args[1])))), nil
}

func numberArg(v any) any {
	switch x := v.(type) {
	case int64, float64:
		return x
	}
	s, _ := textArg(v)
	return parseNumber(s)
}

// timestampAdd(unit, v, d) is TIMESTAMPADD: DATE_ADD(d, INTERVAL v unit).
func timestampAdd(args []any) (any, error) {
	return dateAdd([]any{args[2], args[1], args[0], int64(1)})
}

package udf

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"mysqlite/internal/tz"
)

// Date and time functions (docs/05-dialect.md section 5, ADR 0007). Arguments are the
// SQLite values of MySQL temporal values: 'YYYY-MM-DD[ hh:mm:ss[.ffffff]]' text, a time
// 'hh:mm:ss' for the time parts, or a number such as 20240229. A value that is not a
// valid date is NULL, as in MySQL (which also warns, 1292).

// temporal is a parsed date and time; date or clock may be missing.
type temporal struct {
	year, month, day     int
	hour, minute, second int
	micro                int
	hasDate, hasClock    bool
}

func (v temporal) time() time.Time {
	return time.Date(v.year, time.Month(v.month), v.day, v.hour, v.minute, v.second, v.micro*1000, time.UTC)
}

// parseTemporal reads a value as MySQL reads a DATETIME, a DATE or, with clockOnly
// allowed, a TIME. ok is false for anything else, including invalid calendar dates.
func parseTemporal(v any, clockOnly bool) (temporal, bool) {
	switch x := v.(type) {
	case int64:
		return parseNumberTemporal(x)
	case float64:
		return parseNumberTemporal(int64(x))
	case []byte:
		v = string(x)
	}
	s, ok := v.(string)
	if !ok {
		return temporal{}, false
	}
	s = strings.TrimSpace(s)
	datePart, clockPart, hasSpace := strings.Cut(s, " ")
	if !hasSpace {
		datePart, clockPart, hasSpace = strings.Cut(s, "T")
	}
	var t temporal
	if strings.Count(datePart, "-") == 2 {
		f := strings.Split(datePart, "-")
		y, e1 := strconv.Atoi(f[0])
		m, e2 := strconv.Atoi(f[1])
		d, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil || !validCalendar(y, m, d) {
			return temporal{}, false
		}
		t.year, t.month, t.day, t.hasDate = y, m, d, true
		if !hasSpace {
			return t, true
		}
	} else if clockOnly && !hasSpace {
		clockPart = s
	} else {
		return temporal{}, false
	}
	if !parseClock(clockPart, &t, !t.hasDate) {
		return temporal{}, false
	}
	return t, true
}

func parseClock(s string, t *temporal, allowLong bool) bool {
	whole, frac, _ := strings.Cut(s, ".")
	f := strings.Split(whole, ":")
	if len(f) < 2 || len(f) > 3 {
		return false
	}
	nums := [3]int{}
	for i, p := range f {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return false
		}
		nums[i] = n
	}
	if nums[1] > 59 || nums[2] > 59 || nums[0] > 23 && !allowLong || nums[0] > 838 {
		return false
	}
	if frac != "" {
		if len(frac) > 6 {
			frac = frac[:6]
		}
		us, err := strconv.Atoi(frac + strings.Repeat("0", 6-len(frac)))
		if err != nil {
			return false
		}
		t.micro = us
	}
	t.hour, t.minute, t.second, t.hasClock = nums[0], nums[1], nums[2], true
	return true
}

func parseNumberTemporal(n int64) (temporal, bool) {
	var t temporal
	switch {
	case n >= 10000101 && n <= 99991231:
		t.year, t.month, t.day = int(n/10000), int(n/100%100), int(n%100)
	case n >= 10000101000000 && n <= 99991231235959:
		d, c := n/1000000, n%1000000
		t.year, t.month, t.day = int(d/10000), int(d/100%100), int(d%100)
		t.hour, t.minute, t.second, t.hasClock = int(c/10000), int(c/100%100), int(c%100), true
		if t.hour > 23 || t.minute > 59 || t.second > 59 {
			return temporal{}, false
		}
	default:
		return temporal{}, false
	}
	t.hasDate = validCalendar(t.year, t.month, t.day)
	return t, t.hasDate
}

func validCalendar(y, m, d int) bool {
	if m < 1 || m > 12 || d < 1 || y < 0 || y > 9999 {
		return false
	}
	return d <= daysIn(y, m)
}

func daysIn(y, m int) int {
	return time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// datePart(v, part) is YEAR(v), MONTH(v), ... as an integer.
func datePart(args []any) (any, error) {
	part, _ := textArg(args[1])
	clock := part == "HOUR" || part == "MINUTE" || part == "SECOND" || part == "MICROSECOND"
	t, ok := parseTemporal(args[0], clock)
	if !ok || !clock && !t.hasDate {
		return nil, nil
	}
	switch part {
	case "YEAR":
		return int64(t.year), nil
	case "MONTH":
		return int64(t.month), nil
	case "DAY":
		return int64(t.day), nil
	case "HOUR":
		return int64(t.hour), nil
	case "MINUTE":
		return int64(t.minute), nil
	case "SECOND":
		return int64(t.second), nil
	case "MICROSECOND":
		return int64(t.micro), nil
	case "QUARTER":
		return int64((t.month + 2) / 3), nil
	case "DAYOFWEEK":
		return int64(t.time().Weekday()) + 1, nil
	case "WEEKDAY":
		return int64((t.time().Weekday() + 6) % 7), nil
	case "DAYOFYEAR":
		return int64(t.time().YearDay()), nil
	}
	return nil, fmt.Errorf("udf: date part %q", part)
}

// dateName(v, 'DAY'|'MONTH') is DAYNAME and MONTHNAME (lc_time_names en_US).
func dateName(args []any) (any, error) {
	t, ok := parseTemporal(args[0], false)
	if !ok {
		return nil, nil
	}
	if part, _ := textArg(args[1]); part == "MONTH" {
		return time.Month(t.month).String(), nil
	}
	return t.time().Weekday().String(), nil
}

// dateOnly(v, 'DATE'|'LAST_DAY') is DATE(v) and LAST_DAY(v).
func dateOnly(args []any) (any, error) {
	t, ok := parseTemporal(args[0], false)
	if !ok {
		return nil, nil
	}
	if part, _ := textArg(args[1]); part == "LAST_DAY" {
		t.day = daysIn(t.year, t.month)
	}
	return fmt.Sprintf("%04d-%02d-%02d", t.year, t.month, t.day), nil
}

// dateDiff(a, b) is DATEDIFF: days between the dates, times ignored.
func dateDiff(args []any) (any, error) {
	a, ok1 := parseTemporal(args[0], false)
	b, ok2 := parseTemporal(args[1], false)
	if !ok1 || !ok2 {
		return nil, nil
	}
	da := time.Date(a.year, time.Month(a.month), a.day, 0, 0, 0, 0, time.UTC)
	db := time.Date(b.year, time.Month(b.month), b.day, 0, 0, 0, 0, time.UTC)
	return int64(da.Sub(db).Hours() / 24), nil
}

// unixTimestamp(v, zone) is UNIX_TIMESTAMP(v): v read in the session zone. A fraction
// is dropped (MySQL returns a DECIMAL then; discrepancies).
func unixTimestamp(args []any) (any, error) {
	t, ok := parseTemporal(args[0], false)
	if !ok {
		return nil, nil
	}
	zone, _ := textArg(args[1])
	loc, err := tz.Location(zone)
	if err != nil {
		return nil, err
	}
	u := time.Date(t.year, time.Month(t.month), t.day, t.hour, t.minute, t.second, 0, loc).Unix()
	if u < 0 {
		return int64(0), nil
	}
	return u, nil
}

// fromUnixtime(n, format, zone) is FROM_UNIXTIME(n[, format]) in the session zone.
func fromUnixtime(args []any) (any, error) {
	var secs int64
	switch x := args[0].(type) {
	case nil:
		return nil, nil
	case int64:
		secs = x
	case float64:
		secs = int64(x)
	case string:
		secs = int64(toFloat(parseNumber(x)))
	default:
		return nil, nil
	}
	if secs < 0 {
		return nil, nil
	}
	zone, _ := textArg(args[2])
	loc, err := tz.Location(zone)
	if err != nil {
		return nil, err
	}
	lt := time.Unix(secs, 0).In(loc)
	if args[1] == nil {
		return lt.Format("2006-01-02 15:04:05"), nil
	}
	format, _ := textArg(args[1])
	t := temporal{year: lt.Year(), month: int(lt.Month()), day: lt.Day(), hour: lt.Hour(), minute: lt.Minute(), second: lt.Second(), hasDate: true, hasClock: true}
	return formatDate(t, format), nil
}

// dateFormat(v, format) is DATE_FORMAT.
func dateFormat(args []any) (any, error) {
	if args[1] == nil {
		return nil, nil
	}
	t, ok := parseTemporal(args[0], false)
	if !ok {
		return nil, nil
	}
	format, _ := textArg(args[1])
	return formatDate(t, format), nil
}

// formatDate applies MySQL's DATE_FORMAT specifiers; an unknown one prints its letter.
func formatDate(t temporal, format string) string {
	var b strings.Builder
	tt := t.time()
	hour12 := t.hour % 12
	if hour12 == 0 {
		hour12 = 12
	}
	ampm := "AM"
	if t.hour%24 >= 12 {
		ampm = "PM"
	}
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' || i+1 == len(format) {
			b.WriteByte(c)
			continue
		}
		i++
		switch format[i] {
		case 'a':
			b.WriteString(tt.Weekday().String()[:3])
		case 'b':
			b.WriteString(time.Month(t.month).String()[:3])
		case 'c':
			b.WriteString(strconv.Itoa(t.month))
		case 'D':
			b.WriteString(strconv.Itoa(t.day) + ordinal(t.day))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.day)
		case 'e':
			b.WriteString(strconv.Itoa(t.day))
		case 'f':
			fmt.Fprintf(&b, "%06d", t.micro)
		case 'H':
			fmt.Fprintf(&b, "%02d", t.hour)
		case 'h', 'I':
			fmt.Fprintf(&b, "%02d", hour12)
		case 'i':
			fmt.Fprintf(&b, "%02d", t.minute)
		case 'j':
			fmt.Fprintf(&b, "%03d", tt.YearDay())
		case 'k':
			b.WriteString(strconv.Itoa(t.hour))
		case 'l':
			b.WriteString(strconv.Itoa(hour12))
		case 'M':
			b.WriteString(time.Month(t.month).String())
		case 'm':
			fmt.Fprintf(&b, "%02d", t.month)
		case 'p':
			b.WriteString(ampm)
		case 'r':
			fmt.Fprintf(&b, "%02d:%02d:%02d %s", hour12, t.minute, t.second, ampm)
		case 'S', 's':
			fmt.Fprintf(&b, "%02d", t.second)
		case 'T':
			fmt.Fprintf(&b, "%02d:%02d:%02d", t.hour, t.minute, t.second)
		case 'U':
			w, _ := weekMode(tt, 0)
			fmt.Fprintf(&b, "%02d", w)
		case 'u':
			w, _ := weekMode(tt, 1)
			fmt.Fprintf(&b, "%02d", w)
		case 'V':
			w, _ := weekMode(tt, 2)
			fmt.Fprintf(&b, "%02d", w)
		case 'v':
			w, _ := weekMode(tt, 3)
			fmt.Fprintf(&b, "%02d", w)
		case 'X':
			_, y := weekMode(tt, 2)
			fmt.Fprintf(&b, "%04d", y)
		case 'x':
			_, y := weekMode(tt, 3)
			fmt.Fprintf(&b, "%04d", y)
		case 'W':
			b.WriteString(tt.Weekday().String())
		case 'w':
			b.WriteString(strconv.Itoa(int(tt.Weekday())))
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.year)
		case 'y':
			fmt.Fprintf(&b, "%02d", t.year%100)
		default:
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

func ordinal(day int) string {
	if day >= 11 && day <= 13 {
		return "th"
	}
	switch day % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	}
	return "th"
}

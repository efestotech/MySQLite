package udf

import (
	"fmt"
	"strings"
	"time"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// strDate is the state STR_TO_DATE builds while it reads a value.
type strDate struct {
	t         temporal
	hour12    bool
	pm, am    bool
	dayOfYear int
}

// strToDate(s, format) is STR_TO_DATE under NO_ZERO_DATE and NO_ZERO_IN_DATE (the
// dialect refuses it otherwise): a value that does not match the format, an invalid
// date, or a date with a zero part is NULL. Characters after the format are ignored, as
// MySQL ignores them (with a warning mysqlite does not send).
func strToDate(args []any) (any, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	s, ok1 := textArg(args[0])
	format, ok2 := textArg(args[1])
	if !ok1 || !ok2 {
		s, format = fmt.Sprint(args[0]), fmt.Sprint(args[1])
	}
	kind, fsp, ok := meta.StrToDateKind(format)
	if !ok {
		return nil, errs.FunctionUnsupported("STR_TO_DATE with format '" + format + "'")
	}
	var d strDate
	if _, ok := d.read(s, format); !ok {
		return nil, nil
	}
	t := &d.t
	if d.hour12 {
		if t.hour < 1 || t.hour > 12 {
			return nil, nil
		}
		t.hour %= 12
		if d.pm {
			t.hour += 12
		}
	}
	if t.hour > 23 || t.minute > 59 || t.second > 59 {
		return nil, nil
	}
	if d.dayOfYear > 0 {
		if t.year < 1 || d.dayOfYear > time.Date(t.year, 12, 31, 0, 0, 0, 0, time.UTC).YearDay() {
			return nil, nil
		}
		day := time.Date(t.year, 1, d.dayOfYear, 0, 0, 0, 0, time.UTC)
		t.month, t.day = int(day.Month()), day.Day()
	}
	clock := fmt.Sprintf("%02d:%02d:%02d", t.hour, t.minute, t.second)
	if fsp > 0 {
		clock += fmt.Sprintf(".%06d", t.micro)
	}
	if kind == meta.RetTime {
		return clock, nil
	}
	if !validCalendar(t.year, t.month, t.day) {
		return nil, nil
	}
	date := fmt.Sprintf("%04d-%02d-%02d", t.year, t.month, t.day)
	if kind == meta.RetDate {
		return date, nil
	}
	return date + " " + clock, nil
}

// read matches s against format from the start and returns what is left of s. Spaces
// in the format are ignored and spaces in s are skipped before each format element,
// as MySQL reads them.
func (d *strDate) read(s, format string) (string, bool) {
	for i := 0; i < len(format) && s != ""; i++ {
		if s = strings.TrimLeft(s, " \t\n\r"); s == "" {
			break
		}
		c := format[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		if c != '%' || i+1 == len(format) {
			if s == "" || s[0] != c {
				return s, false
			}
			s = s[1:]
			continue
		}
		i++
		var ok bool
		t := &d.t
		switch format[i] {
		case 'Y':
			var n, w int
			n, w, s, ok = digits(s, 4)
			if w <= 2 {
				n = twoDigitYear(n)
			}
			t.year = n
		case 'y':
			var n int
			n, _, s, ok = digits(s, 2)
			t.year = twoDigitYear(n)
		case 'm', 'c':
			t.month, _, s, ok = digits(s, 2)
		case 'd', 'e':
			t.day, _, s, ok = digits(s, 2)
		case 'D':
			t.day, _, s, ok = digits(s, 2)
			if ok && len(s) >= 2 {
				s = s[2:]
			} else {
				ok = false
			}
		case 'j':
			d.dayOfYear, _, s, ok = digits(s, 3)
			ok = ok && d.dayOfYear > 0
		case 'M':
			t.month, s, ok = name(s, monthNames, 0)
		case 'b':
			t.month, s, ok = name(s, monthNames, 3)
		case 'W':
			_, s, ok = name(s, dayNames, 0)
		case 'a':
			_, s, ok = name(s, dayNames, 3)
		case 'H', 'k':
			t.hour, _, s, ok = digits(s, 2)
		case 'h', 'I', 'l':
			t.hour, _, s, ok = digits(s, 2)
			d.hour12 = true
		case 'i':
			t.minute, _, s, ok = digits(s, 2)
		case 's', 'S':
			t.second, _, s, ok = digits(s, 2)
		case 'f':
			var n, w int
			n, w, s, ok = digits(s, 6)
			for ; w < 6; w++ {
				n *= 10
			}
			t.micro = n
		case 'p':
			switch {
			case len(s) >= 2 && strings.EqualFold(s[:2], "AM"):
				d.am = true
			case len(s) >= 2 && strings.EqualFold(s[:2], "PM"):
				d.pm = true
			default:
				return s, false
			}
			s, ok = s[2:], true
		case 'T':
			s, ok = d.read(s, "%H:%i:%S")
		case 'r':
			s, ok = d.read(s, "%I:%i:%S %p")
		case '%':
			ok = s[0] == '%'
			if ok {
				s = s[1:]
			}
		}
		if !ok {
			return s, false
		}
	}
	return s, true
}

// digits reads 1 to most decimal digits.
func digits(s string, most int) (n, width int, rest string, ok bool) {
	for width < most && width < len(s) && s[width] >= '0' && s[width] <= '9' {
		n = n*10 + int(s[width]-'0')
		width++
	}
	return n, width, s[width:], width > 0
}

// twoDigitYear maps 00-69 to 2000-2069 and 70-99 to 1970-1999.
func twoDigitYear(n int) int {
	if n < 70 {
		return n + 2000
	}
	return n + 1900
}

var (
	monthNames = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	dayNames   = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
)

// name reads one of names, whole or abbreviated to its first abbrev letters, without
// regard to case, and returns its 1-based position.
func name(s string, names []string, abbrev int) (int, string, bool) {
	for i, n := range names {
		if abbrev > 0 {
			n = n[:abbrev]
		}
		if len(s) >= len(n) && strings.EqualFold(s[:len(n)], n) {
			return i + 1, s[len(n):], true
		}
	}
	return 0, s, false
}

// timeFormat(t, format) is TIME_FORMAT: DATE_FORMAT's clock specifiers over a TIME,
// whose hours may exceed 23. The dialect refuses the other specifiers; a negative time
// is refused here.
func timeFormat(args []any) (any, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	if s, ok := textArg(args[0]); ok && strings.HasPrefix(strings.TrimSpace(s), "-") {
		return nil, errs.FunctionUnsupported("TIME_FORMAT of a negative time")
	}
	t, ok := parseTemporal(args[0], true)
	if !ok {
		return nil, nil
	}
	format, _ := textArg(args[1])
	if !meta.TimeFormatOK(format) {
		return nil, errs.FunctionUnsupported("TIME_FORMAT with a date specifier")
	}
	return formatDate(t, format), nil
}

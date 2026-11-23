package meta

import (
	"regexp"
	"strconv"

	"mysqlite/internal/errs"
)

// reCanonicalTemporal is the canonical text of a DATE, DATETIME or TIMESTAMP value:
// YYYY-MM-DD, optionally followed by hh:mm:ss[.ffffff].
var reCanonicalTemporal = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?: (\d{2}):(\d{2}):(\d{2})(?:\.\d{1,6})?)?$`)

// checkTemporal is the strict-mode check of a string literal written into a DATE,
// DATETIME or TIMESTAMP column: 1292 for the zero date under NO_ZERO_DATE, a zero month
// or day under NO_ZERO_IN_DATE, a day the month does not have (unless
// ALLOW_INVALID_DATES, which still limits days to 31 and months to 12) and a time of
// day out of range. Only the canonical text is checked; other spellings MySQL accepts
// ('20240102', '2024-1-2') are stored as given, which docs/agent-notes/discrepancies.md lists as a gap.
func checkTemporal(ty uint8, column string, v *Literal, row int, mode InsertMode) *errs.Error {
	if !mode.Strict || v == nil || v.Kind != LiteralString {
		return nil
	}
	kind := "datetime"
	switch ty {
	case TypeDate:
		kind = "date"
	case TypeDatetime, TypeTimestamp:
	default:
		return nil
	}
	m := reCanonicalTemporal.FindStringSubmatch(v.Text)
	if m == nil || ty == TypeDate && m[4] != "" {
		return nil
	}
	num := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	year, month, day := num(m[1]), num(m[2]), num(m[3])
	bad := errs.IncorrectTemporal(kind, v.Text, column, row)
	if year == 0 && month == 0 && day == 0 {
		if mode.NoZeroDate {
			return bad
		}
	} else if month == 0 || day == 0 {
		if mode.NoZeroInDate {
			return bad
		}
	}
	if month > 12 || day > 31 || !mode.AllowInvalidDates && month > 0 && day > daysIn(year, month) {
		return bad
	}
	if m[4] != "" && (num(m[4]) > 23 || num(m[5]) > 59 || num(m[6]) > 59) {
		return bad
	}
	return nil
}

// daysIn is the number of days of month in year (proleptic Gregorian).
func daysIn(year, month int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

package vars

import (
	"math/big"
	"strings"

	"mysqlite/internal/errs"
)

// sqlModes are MySQL 8.0's sql_mode names in bit order; "" marks unused bits. MySQL
// prints a mode in this order whatever order it was set in.
var sqlModes = []string{
	"REAL_AS_FLOAT", "PIPES_AS_CONCAT", "ANSI_QUOTES", "IGNORE_SPACE", "",
	"ONLY_FULL_GROUP_BY", "NO_UNSIGNED_SUBTRACTION", "NO_DIR_IN_CREATE",
	"", "", "", "", "", "", "", "", "", "",
	"ANSI", "NO_AUTO_VALUE_ON_ZERO", "NO_BACKSLASH_ESCAPES", "STRICT_TRANS_TABLES",
	"STRICT_ALL_TABLES", "NO_ZERO_IN_DATE", "NO_ZERO_DATE", "ALLOW_INVALID_DATES",
	"ERROR_FOR_DIVISION_BY_ZERO", "TRADITIONAL", "", "HIGH_NOT_PRECEDENCE",
	"NO_ENGINE_SUBSTITUTION", "PAD_CHAR_TO_FULL_LENGTH", "TIME_TRUNCATE_FRACTIONAL",
}

// Combination modes add their members and keep their own name, as MySQL does.
var sqlModeCombos = map[string][]string{
	"ANSI":        {"REAL_AS_FLOAT", "PIPES_AS_CONCAT", "ANSI_QUOTES", "IGNORE_SPACE", "ONLY_FULL_GROUP_BY"},
	"TRADITIONAL": {"STRICT_TRANS_TABLES", "STRICT_ALL_TABLES", "NO_ZERO_IN_DATE", "NO_ZERO_DATE", "ERROR_FOR_DIVISION_BY_ZERO", "NO_ENGINE_SUBSTITUTION"},
}

// honouredModes change mysqlite's behaviour when set or cleared. Every other mode is
// stored and reported only; changing one raises a warning.
var honouredModes = map[string]bool{
	"PIPES_AS_CONCAT": true, "ANSI_QUOTES": true, "IGNORE_SPACE": true,
	"ONLY_FULL_GROUP_BY": true, "ANSI": true, "TRADITIONAL": true,
	"NO_AUTO_VALUE_ON_ZERO": true, "NO_BACKSLASH_ESCAPES": true,
	"NO_ENGINE_SUBSTITUTION": true,
	// mysqlite is always strict; leaving both out raises its own warning.
	"STRICT_TRANS_TABLES": true, "STRICT_ALL_TABLES": true,
}

// NormalizeSQLMode validates a comma-separated sql_mode and returns it as MySQL prints
// it, with the warnings for modes mysqlite does not apply.
func NormalizeSQLMode(text string) (string, []*errs.Error, *errs.Error) {
	set := map[string]bool{}
	for _, part := range strings.Split(text, ",") {
		name := strings.ToUpper(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !validMode(name) {
			return "", nil, errs.WrongValueForVariable("sql_mode", name)
		}
		set[name] = true
		for _, m := range sqlModeCombos[name] {
			set[m] = true
		}
	}
	return renderMode(set), modeWarnings(set), nil
}

// sqlModeFromBits decodes SET sql_mode = <integer>.
func sqlModeFromBits(text string) (string, []*errs.Error, *errs.Error) {
	n, ok := new(big.Int).SetString(text, 10)
	if !ok || n.Sign() < 0 || n.BitLen() > len(sqlModes) {
		return "", nil, errs.WrongValueForVariable("sql_mode", text)
	}
	set := map[string]bool{}
	for i, name := range sqlModes {
		if n.Bit(i) == 0 {
			continue
		}
		if name == "" {
			return "", nil, errs.WrongValueForVariable("sql_mode", text)
		}
		set[name] = true
		for _, m := range sqlModeCombos[name] {
			set[m] = true
		}
	}
	return renderMode(set), modeWarnings(set), nil
}

func validMode(name string) bool {
	for _, m := range sqlModes {
		if m != "" && m == name {
			return true
		}
	}
	return false
}

func renderMode(set map[string]bool) string {
	var out []string
	for _, m := range sqlModes {
		if m != "" && set[m] {
			out = append(out, m)
		}
	}
	return strings.Join(out, ",")
}

func modeWarnings(set map[string]bool) []*errs.Error {
	def := map[string]bool{}
	for _, m := range strings.Split(DefaultSQLMode, ",") {
		def[m] = true
	}
	var changed []string
	for _, m := range sqlModes {
		if m != "" && !honouredModes[m] && set[m] != def[m] {
			changed = append(changed, m)
		}
	}
	var warnings []*errs.Error
	if !set["STRICT_TRANS_TABLES"] && !set["STRICT_ALL_TABLES"] {
		warnings = append(warnings, errs.Unsupported("sql_mode without STRICT_TRANS_TABLES", "mysqlite stays strict"))
	}
	if len(changed) > 0 {
		warnings = append(warnings, errs.Unsupported("sql_mode "+strings.Join(changed, ","), "the mode is stored and reported, mysqlite does not apply it"))
	}
	return warnings
}

// HasMode reports whether a normalized sql_mode contains name.
func HasMode(mode, name string) bool {
	for _, m := range strings.Split(mode, ",") {
		if m == name {
			return true
		}
	}
	return false
}

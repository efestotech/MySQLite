package meta

import (
	"math"
	"strconv"
	"strings"
)

// TypeInfo is the part of a column definition that follows from the declared type.
type TypeInfo struct {
	Type     uint8
	Length   uint32
	Decimals uint8
	Charset  uint16
	Flags    uint16
}

// ParseType reads a MySQL column type such as "DECIMAL(10,2) UNSIGNED" or
// "VARCHAR(20)". ok is false for a type it does not know or a malformed one; callers
// degrade instead of guessing.
//
// ENUM, SET and character-set or collation clauses are not part of the declared type
// SQLite stores; they come from the sidecar metadata of M2/M3.
func ParseType(decl string) (TypeInfo, bool) {
	toks, ok := tokenize(decl)
	if !ok || len(toks) == 0 {
		return TypeInfo{}, false
	}
	name, i := toks[0], 1
	if i < len(toks) {
		switch name + " " + toks[i] {
		case "DOUBLE PRECISION":
			name, i = "DOUBLE", i+1
		case "CHARACTER VARYING", "CHAR VARYING":
			name, i = "VARCHAR", i+1
		case "NATIONAL CHAR", "NATIONAL CHARACTER":
			name, i = "NCHAR", i+1
		case "NATIONAL VARCHAR":
			name, i = "NVARCHAR", i+1
		}
	}
	if name == "CHARACTER" {
		name = "CHAR"
	}

	var args []int
	if i < len(toks) && toks[i] == "(" {
		i++
		for {
			if i >= len(toks) {
				return TypeInfo{}, false
			}
			n, err := strconv.Atoi(toks[i])
			if err != nil || n < 0 {
				return TypeInfo{}, false
			}
			args = append(args, n)
			i++
			if i < len(toks) && toks[i] == "," {
				i++
				continue
			}
			if i < len(toks) && toks[i] == ")" {
				i++
				break
			}
			return TypeInfo{}, false
		}
	}

	var unsigned, zerofill bool
	for ; i < len(toks); i++ {
		switch toks[i] {
		case "UNSIGNED":
			unsigned = true
		case "SIGNED":
		case "ZEROFILL":
			zerofill, unsigned = true, true
		default:
			return TypeInfo{}, false
		}
	}
	return build(name, args, unsigned, zerofill)
}

func build(name string, args []int, unsigned, zerofill bool) (TypeInfo, bool) {
	numFlags := FlagNum
	if unsigned {
		numFlags |= FlagUnsigned
	}
	if zerofill {
		numFlags |= FlagZerofill
	}
	integer := func(t uint8, signedLen, unsignedLen uint32) (TypeInfo, bool) {
		ti := TypeInfo{Type: t, Length: signedLen, Charset: CharsetBinary, Flags: numFlags}
		if unsigned {
			ti.Length = unsignedLen
		}
		switch len(args) {
		case 0:
		case 1:
			if args[0] < 1 || args[0] > 255 {
				return TypeInfo{}, false
			}
			ti.Length = uint32(args[0])
		default:
			return TypeInfo{}, false
		}
		return ti, true
	}
	numericOnly := !unsigned

	switch name {
	case "TINYINT", "INT1":
		return integer(TypeTiny, 4, 3)
	case "BOOL", "BOOLEAN":
		if len(args) != 0 {
			return TypeInfo{}, false
		}
		return TypeInfo{Type: TypeTiny, Length: 1, Charset: CharsetBinary, Flags: numFlags}, true
	case "SMALLINT", "INT2":
		return integer(TypeShort, 6, 5)
	case "MEDIUMINT", "INT3", "MIDDLEINT":
		return integer(TypeInt24, 9, 8)
	case "INT", "INTEGER", "INT4":
		return integer(TypeLong, 11, 10)
	case "BIGINT", "INT8":
		return integer(TypeLongLong, 20, 20)

	case "FLOAT":
		switch len(args) {
		case 0:
			return TypeInfo{Type: TypeFloat, Length: 12, Decimals: notFixedDecimals, Charset: CharsetBinary, Flags: numFlags}, true
		case 1:
			// FLOAT(p): single precision up to 24 bits, double up to 53.
			switch {
			case args[0] <= 24:
				return TypeInfo{Type: TypeFloat, Length: 12, Decimals: notFixedDecimals, Charset: CharsetBinary, Flags: numFlags}, true
			case args[0] <= 53:
				return TypeInfo{Type: TypeDouble, Length: 22, Decimals: notFixedDecimals, Charset: CharsetBinary, Flags: numFlags}, true
			}
			return TypeInfo{}, false
		case 2:
			return fixedFloat(TypeFloat, args, numFlags)
		}
		return TypeInfo{}, false
	case "DOUBLE", "REAL", "FLOAT8":
		switch len(args) {
		case 0:
			return TypeInfo{Type: TypeDouble, Length: 22, Decimals: notFixedDecimals, Charset: CharsetBinary, Flags: numFlags}, true
		case 2:
			return fixedFloat(TypeDouble, args, numFlags)
		}
		return TypeInfo{}, false
	case "DECIMAL", "DEC", "NUMERIC", "FIXED":
		m, d := 10, 0
		switch len(args) {
		case 0:
		case 1:
			m = args[0]
		case 2:
			m, d = args[0], args[1]
		default:
			return TypeInfo{}, false
		}
		if m < 1 || m > 65 || d > 30 || d > m {
			return TypeInfo{}, false
		}
		return TypeInfo{Type: TypeNewDecimal, Length: uint32(decimalLength(m, d, unsigned)), Decimals: uint8(d), Charset: CharsetBinary, Flags: numFlags}, true
	case "BIT":
		if !numericOnly {
			return TypeInfo{}, false
		}
		n := 1
		switch len(args) {
		case 0:
		case 1:
			n = args[0]
		default:
			return TypeInfo{}, false
		}
		if n < 1 || n > 64 {
			return TypeInfo{}, false
		}
		return TypeInfo{Type: TypeBit, Length: uint32(n), Charset: CharsetBinary, Flags: FlagUnsigned}, true
	case "YEAR":
		if !numericOnly || len(args) > 1 || (len(args) == 1 && args[0] != 4) {
			return TypeInfo{}, false
		}
		return TypeInfo{Type: TypeYear, Length: 4, Charset: CharsetBinary, Flags: FlagUnsigned | FlagZerofill | FlagNum}, true
	}

	// The remaining types take no numeric modifiers.
	if !numericOnly {
		return TypeInfo{}, false
	}
	switch name {
	case "CHAR", "NCHAR":
		return charType(TypeString, name == "NCHAR", args, false)
	case "VARCHAR", "NVARCHAR":
		return charType(TypeVarString, name == "NVARCHAR", args, true)
	case "BINARY":
		return binaryType(TypeString, args, false)
	case "VARBINARY":
		return binaryType(TypeVarString, args, true)

	case "TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT":
		chars, ok := lobSize(name, "TEXT", args)
		if !ok {
			return TypeInfo{}, false
		}
		return TypeInfo{Type: TypeBlob, Length: capLength(chars * 4), Charset: CollationDefault, Flags: FlagBlob}, true
	case "TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB":
		n, ok := lobSize(name, "BLOB", args)
		if !ok {
			return TypeInfo{}, false
		}
		return TypeInfo{Type: TypeBlob, Length: capLength(n), Charset: CharsetBinary, Flags: FlagBlob | FlagBinary}, true

	case "DATE":
		if len(args) != 0 {
			return TypeInfo{}, false
		}
		return TypeInfo{Type: TypeDate, Length: 10, Charset: CharsetBinary, Flags: FlagBinary}, true
	case "TIME":
		return temporal(TypeTime, 10, args, FlagBinary)
	case "DATETIME":
		return temporal(TypeDatetime, 19, args, FlagBinary)
	case "TIMESTAMP":
		return temporal(TypeTimestamp, 19, args, FlagBinary|FlagTimestamp)
	case "JSON":
		if len(args) != 0 {
			return TypeInfo{}, false
		}
		return TypeInfo{Type: TypeJSON, Length: math.MaxUint32, Charset: CharsetBinary, Flags: FlagBlob | FlagBinary}, true
	}
	return TypeInfo{}, false
}

func fixedFloat(t uint8, args []int, flags uint16) (TypeInfo, bool) {
	m, d := args[0], args[1]
	if m < 1 || m > 255 || d > 30 || d > m {
		return TypeInfo{}, false
	}
	return TypeInfo{Type: t, Length: uint32(m), Decimals: uint8(d), Charset: CharsetBinary, Flags: flags}, true
}

func charType(t uint8, national bool, args []int, required bool) (TypeInfo, bool) {
	n := 1
	switch {
	case len(args) == 1:
		n = args[0]
	case len(args) > 1 || required:
		return TypeInfo{}, false
	}
	limit := 255
	if t == TypeVarString {
		limit = 65535
	}
	if n > limit {
		return TypeInfo{}, false
	}
	if national {
		return TypeInfo{Type: t, Length: uint32(n * 3), Charset: CollationUTF8MB3}, true
	}
	return TypeInfo{Type: t, Length: uint32(n * 4), Charset: CollationDefault}, true
}

func binaryType(t uint8, args []int, required bool) (TypeInfo, bool) {
	n := 1
	switch {
	case len(args) == 1:
		n = args[0]
	case len(args) > 1 || required:
		return TypeInfo{}, false
	}
	if (t == TypeString && n > 255) || n > 65535 {
		return TypeInfo{}, false
	}
	return TypeInfo{Type: t, Length: uint32(n), Charset: CharsetBinary, Flags: FlagBinary}, true
}

// lobSize returns the maximum size of a TEXT (characters) or BLOB (bytes) type.
// TEXT(n) and BLOB(n) pick the smallest type that holds n.
func lobSize(name, base string, args []int) (int, bool) {
	sizes := map[string]int{"TINY" + base: 255, base: 65535, "MEDIUM" + base: 16777215, "LONG" + base: math.MaxUint32}
	switch {
	case len(args) == 0:
		return sizes[name], true
	case len(args) == 1 && name == base:
		for _, s := range []int{255, 65535, 16777215} {
			if args[0] <= s {
				return s, true
			}
		}
		return math.MaxUint32, true
	}
	return 0, false
}

func capLength(n int) uint32 {
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}

// temporal handles TIME, DATETIME and TIMESTAMP with an optional fractional-seconds
// precision of 0-6, which adds the point and the digits to the length.
func temporal(t uint8, base uint32, args []int, flags uint16) (TypeInfo, bool) {
	fsp := 0
	switch len(args) {
	case 0:
	case 1:
		fsp = args[0]
	default:
		return TypeInfo{}, false
	}
	if fsp > 6 {
		return TypeInfo{}, false
	}
	length := base
	if fsp > 0 {
		length += uint32(fsp) + 1
	}
	return TypeInfo{Type: t, Length: length, Decimals: uint8(fsp), Charset: CharsetBinary, Flags: flags}, true
}

// tokenize splits a declared type into upper-case words, numbers and the punctuation
// "(", ")" and ",". Any other character makes the type unrecognized.
func tokenize(s string) ([]string, bool) {
	var toks []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == ',':
			toks = append(toks, string(c))
			i++
		case c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(s) && (s[j] == '_' || s[j] >= '0' && s[j] <= '9' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
				j++
			}
			toks = append(toks, strings.ToUpper(s[i:j]))
			i = j
		default:
			return nil, false
		}
	}
	return toks, true
}

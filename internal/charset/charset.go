// Package charset is the static table of the character sets and collations mysqlite
// accepts (docs/04-session-catalog.md sections 2 and 4). Everything is stored and sent
// as UTF-8, so only charsets whose bytes are UTF-8 (or raw bytes, binary) are supported;
// transcoding (latin1) is not implemented yet.
package charset

import "strings"

// Charset is one supported character set.
type Charset struct {
	Name             string
	Description      string
	DefaultCollation string
	MaxLen           int
}

// Collation is one supported collation.
type Collation struct {
	Name    string
	Charset string
	ID      uint16
	Default bool
	// PadAttribute is "PAD SPACE" or "NO PAD".
	PadAttribute string
}

// Charsets lists the supported character sets, by name.
var Charsets = []Charset{
	{Name: "binary", Description: "Binary pseudo charset", DefaultCollation: "binary", MaxLen: 1},
	{Name: "utf8mb3", Description: "UTF-8 Unicode", DefaultCollation: "utf8mb3_general_ci", MaxLen: 3},
	// The project's default collation is utf8mb4_general_ci (docs/00-brief.md section 6);
	// MySQL 8.0's is utf8mb4_0900_ai_ci.
	{Name: "utf8mb4", Description: "UTF-8 Unicode", DefaultCollation: "utf8mb4_general_ci", MaxLen: 4},
}

// Collations lists the supported collations, by name. IDs are MySQL's.
var Collations = []Collation{
	{Name: "binary", Charset: "binary", ID: 63, Default: true, PadAttribute: "NO PAD"},
	{Name: "utf8mb3_bin", Charset: "utf8mb3", ID: 83, PadAttribute: "PAD SPACE"},
	{Name: "utf8mb3_general_ci", Charset: "utf8mb3", ID: 33, Default: true, PadAttribute: "PAD SPACE"},
	{Name: "utf8mb3_unicode_ci", Charset: "utf8mb3", ID: 192, PadAttribute: "PAD SPACE"},
	{Name: "utf8mb4_0900_ai_ci", Charset: "utf8mb4", ID: 255, PadAttribute: "NO PAD"},
	{Name: "utf8mb4_0900_bin", Charset: "utf8mb4", ID: 309, PadAttribute: "NO PAD"},
	{Name: "utf8mb4_bin", Charset: "utf8mb4", ID: 46, PadAttribute: "PAD SPACE"},
	{Name: "utf8mb4_general_ci", Charset: "utf8mb4", ID: 45, Default: true, PadAttribute: "PAD SPACE"},
	{Name: "utf8mb4_unicode_ci", Charset: "utf8mb4", ID: 224, PadAttribute: "PAD SPACE"},
}

// known lists names MySQL knows but mysqlite cannot serve: they need transcoding.
var known = map[string]bool{
	"armscii8": true, "ascii": true, "big5": true, "cp1250": true, "cp1251": true,
	"cp1256": true, "cp1257": true, "cp850": true, "cp852": true, "cp866": true,
	"cp932": true, "dec8": true, "eucjpms": true, "euckr": true, "gb18030": true,
	"gb2312": true, "gbk": true, "geostd8": true, "greek": true, "hebrew": true,
	"hp8": true, "keybcs2": true, "koi8r": true, "koi8u": true, "latin1": true,
	"latin2": true, "latin5": true, "latin7": true, "macce": true, "macroman": true,
	"sjis": true, "swe7": true, "tis620": true, "ucs2": true, "ujis": true,
	"utf16": true, "utf16le": true, "utf32": true,
}

// Normalize maps aliases to the canonical name: utf8 is utf8mb3 in MySQL 8.0.
func Normalize(name string) string {
	name = strings.ToLower(name)
	switch name {
	case "utf8":
		return "utf8mb3"
	}
	if strings.HasPrefix(name, "utf8_") {
		return "utf8mb3_" + strings.TrimPrefix(name, "utf8_")
	}
	return name
}

// Lookup finds a supported charset by name or alias.
func Lookup(name string) (*Charset, bool) {
	name = Normalize(name)
	for i := range Charsets {
		if Charsets[i].Name == name {
			return &Charsets[i], true
		}
	}
	return nil, false
}

// Known reports a charset MySQL has but mysqlite does not support.
func Known(name string) bool {
	return known[Normalize(name)]
}

// LookupCollation finds a supported collation by name or alias.
func LookupCollation(name string) (*Collation, bool) {
	name = Normalize(name)
	for i := range Collations {
		if Collations[i].Name == name {
			return &Collations[i], true
		}
	}
	return nil, false
}

// Binary reports a collation that compares by code point or byte: binary and the _bin
// collations. Unknown IDs are not binary.
func Binary(id uint16) bool {
	for _, c := range Collations {
		if c.ID == id {
			return c.Name == "binary" || strings.HasSuffix(c.Name, "_bin")
		}
	}
	return false
}

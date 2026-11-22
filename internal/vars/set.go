package vars

import (
	"math/big"
	"strconv"
	"strings"

	"mysqlite/internal/charset"
	"mysqlite/internal/errs"
)

// Write says how SET may change a variable.
type Write int

const (
	// ReadOnly variables refuse SET with 1238.
	ReadOnly Write = iota
	// Writable variables take SET in every scope they have.
	Writable
	// GlobalWritable variables have a read-only session value: SET SESSION is 1621.
	GlobalWritable
)

// rule is how SET treats one variable. A variable without a rule is read-only.
type rule struct {
	write Write
	// values are the accepted words of an enumeration, as MySQL spells them.
	values []string
	// min and max bound an unsigned variable when max > 0; values outside are clamped
	// with warning 1292, as MySQL does.
	min, max uint64
	// fixed variables accept only their default: mysqlite does not apply other values,
	// and ignoring them would change results without an error.
	fixed string
	// advisory is the warning raised when the variable is set away from its default:
	// the value is stored and reported, but mysqlite does not act on it.
	advisory string
}

const (
	maxTimeout       = 31536000
	reasonNotApplied = "the value is stored and reported, mysqlite does not apply it"
)

var rules = map[string]rule{
	"auto_increment_increment":        {write: Writable, fixed: "generated ids always step by 1"},
	"auto_increment_offset":           {write: Writable, fixed: "generated ids always step by 1"},
	"autocommit":                      {write: Writable},
	"binlog_format":                   {write: Writable, values: []string{"ROW", "STATEMENT", "MIXED"}},
	"character_set_client":            {write: Writable},
	"character_set_connection":        {write: Writable},
	"character_set_database":          {write: Writable},
	"character_set_results":           {write: Writable},
	"character_set_server":            {write: Writable},
	"collation_connection":            {write: Writable, advisory: "string literals compare with utf8mb4_general_ci"},
	"collation_database":              {write: Writable, advisory: "new tables use utf8mb4_general_ci"},
	"collation_server":                {write: Writable, advisory: "new tables use utf8mb4_general_ci"},
	"connect_timeout":                 {write: Writable, min: 2, max: maxTimeout, advisory: reasonNotApplied},
	"default_storage_engine":          {write: Writable},
	"div_precision_increment":         {write: Writable, fixed: "division arrives with M3"},
	"explicit_defaults_for_timestamp": {write: Writable, advisory: "TIMESTAMP columns follow explicit_defaults_for_timestamp=ON"},
	"foreign_key_checks":              {write: Writable, advisory: "foreign keys stay enforced"},
	"group_concat_max_len":            {write: Writable, min: 4, max: 1<<64 - 1},
	"gtid_mode":                       {write: Writable, fixed: "there is no replication"},
	"information_schema_stats_expiry": {write: Writable, min: 0, max: maxTimeout},
	"init_connect":                    {write: Writable, advisory: "init_connect is not run"},
	"interactive_timeout":             {write: Writable, min: 1, max: maxTimeout, advisory: reasonNotApplied},
	"lc_messages":                     {write: Writable, advisory: "messages stay in English"},
	"lc_time_names":                   {write: Writable, advisory: "month and day names stay in English"},
	"max_allowed_packet":              {write: GlobalWritable, fixed: "set max_allowed_packet in mysqlite.yaml"},
	"net_buffer_length":               {write: GlobalWritable, min: 1024, max: 1048576},
	"net_read_timeout":                {write: Writable, min: 1, max: maxTimeout, advisory: reasonNotApplied},
	"net_write_timeout":               {write: Writable, min: 1, max: maxTimeout, advisory: reasonNotApplied},
	"optimizer_switch":                {write: Writable},
	"sql_auto_is_null":                {write: Writable, fixed: "WHERE id IS NULL never finds the last inserted row"},
	"sql_quote_show_create":           {write: Writable, fixed: "SHOW CREATE TABLE always quotes identifiers"},
	"sql_mode":                        {write: Writable},
	"sql_require_primary_key":         {write: Writable, advisory: "tables without a primary key are accepted"},
	"sql_select_limit":                {write: Writable, fixed: "SELECT returns every row"},
	"time_zone":                       {write: Writable},
	"timestamp":                       {write: Writable},
	"transaction_isolation":           {write: Writable, values: []string{"READ-COMMITTED", "REPEATABLE-READ", "SERIALIZABLE"}},
	"transaction_read_only":           {write: Writable},
	"unique_checks":                   {write: Writable},
	"wait_timeout":                    {write: Writable, min: 1, max: maxTimeout, advisory: reasonNotApplied},
}

// Input is the value of one assignment, as written.
type Input struct {
	Default bool
	Null    bool
	// Number is a numeric literal; Integer when it has no fraction or exponent.
	Number, Integer bool
	// Text is the number as written, the string, or the bare word.
	Text string
}

// Change is a checked assignment, ready to apply.
type Change struct {
	Def    *Def
	Global bool
	Value  Value
	// clock resets SET timestamp = DEFAULT to the clock.
	clock bool
}

// Check validates `SET [GLOBAL] name = in` without changing anything, and returns the
// normalized value and the warnings to raise. MySQL checks every assignment of a SET
// before applying any.
func (s *Session) Check(name string, global bool, in Input) (Change, []*errs.Error, *errs.Error) {
	d, ok := Lookup(name)
	if !ok {
		return Change{}, nil, errs.UnknownSystemVariable(name)
	}
	r := rules[d.Name]
	switch {
	case r.write == ReadOnly:
		return Change{}, nil, errs.ReadOnlyVariable(d.Name)
	case global && d.Scope&ScopeGlobal == 0:
		return Change{}, nil, errs.SessionOnlyVariable(d.Name)
	case !global && d.Scope&ScopeSession == 0:
		return Change{}, nil, errs.GlobalOnlyVariable(d.Name)
	case !global && r.write == GlobalWritable:
		return Change{}, nil, errs.SessionReadOnlyVariable(d.Name)
	}
	c := Change{Def: d, Global: global}
	if in.Default {
		switch {
		case d.Name == "timestamp":
			c.clock = true
		case global:
			c.Value = s.srv.defaults[d.Name]
		default:
			c.Value = s.global(d)
		}
		return c, nil, nil
	}
	v, warnings, e := normalize(d, r, in)
	if e != nil {
		return Change{}, nil, e
	}
	c.Value = v
	def := s.srv.defaults[d.Name]
	if r.fixed != "" && v != def {
		return Change{}, nil, errs.Unsupported(d.Name+" = "+quoteValue(v), r.fixed)
	}
	if r.advisory != "" && v != def {
		warnings = append(warnings, errs.Unsupported(d.Name+" = "+quoteValue(v), r.advisory))
	}
	return c, warnings, nil
}

// Apply stores a checked change. Changing a character set also sets the collation that
// goes with it, and the reverse, as MySQL does.
func (s *Session) Apply(c Change) {
	if c.Def.Name == "timestamp" {
		if c.clock {
			s.timestamp = nil
		} else {
			v := c.Value
			s.timestamp = &v
		}
		return
	}
	set := func(name string, v Value) {
		if c.Global {
			s.srv.mu.Lock()
			s.srv.globals[name] = v
			s.srv.mu.Unlock()
			return
		}
		s.values[name] = v
	}
	set(c.Def.Name, c.Value)
	for _, pair := range linked {
		switch c.Def.Name {
		case pair.charset:
			if cs, ok := charset.Lookup(c.Value.Text); ok && !c.Value.Null {
				set(pair.collation, Value{Text: cs.DefaultCollation})
			}
		case pair.collation:
			if co, ok := charset.LookupCollation(c.Value.Text); ok {
				set(pair.charset, Value{Text: co.Charset})
			}
		}
	}
}

var linked = []struct{ charset, collation string }{
	{"character_set_connection", "collation_connection"},
	{"character_set_database", "collation_database"},
	{"character_set_server", "collation_server"},
}

func quoteValue(v Value) string {
	if v.Null {
		return "NULL"
	}
	return "'" + v.Text + "'"
}

func normalize(d *Def, r rule, in Input) (Value, []*errs.Error, *errs.Error) {
	if in.Null {
		if d.Nullable {
			return Value{Null: true}, nil, nil
		}
		return Value{}, nil, errs.WrongValueForVariable(d.Name, "NULL")
	}
	if d.Name == "sql_mode" && in.Number {
		if !in.Integer {
			return Value{}, nil, errs.WrongTypeForVariable(d.Name)
		}
		mode, warnings, e := sqlModeFromBits(in.Text)
		return Value{Text: mode}, warnings, e
	}
	switch d.Kind {
	case KindBool:
		return boolInput(d, in)
	case KindUint, KindInt:
		return uintInput(d, r, in)
	case KindDouble:
		if !in.Number {
			return Value{}, nil, errs.WrongTypeForVariable(d.Name)
		}
		f, err := strconv.ParseFloat(in.Text, 64)
		if err != nil || f < 0 {
			return Value{}, nil, errs.WrongValueForVariable(d.Name, in.Text)
		}
		return Value{Text: strconv.FormatFloat(f, 'f', 6, 64)}, nil, nil
	}
	if in.Number {
		return Value{}, nil, errs.WrongTypeForVariable(d.Name)
	}
	return stringInput(d, r, in.Text)
}

func boolInput(d *Def, in Input) (Value, []*errs.Error, *errs.Error) {
	if in.Number {
		if !in.Integer {
			return Value{}, nil, errs.WrongTypeForVariable(d.Name)
		}
		switch in.Text {
		case "0", "1":
			return Value{Text: in.Text}, nil, nil
		}
		return Value{}, nil, errs.WrongValueForVariable(d.Name, in.Text)
	}
	switch strings.ToUpper(in.Text) {
	case "ON", "TRUE":
		return Value{Text: "1"}, nil, nil
	case "OFF", "FALSE":
		return Value{Text: "0"}, nil, nil
	}
	return Value{}, nil, errs.WrongValueForVariable(d.Name, in.Text)
}

func uintInput(d *Def, r rule, in Input) (Value, []*errs.Error, *errs.Error) {
	if !in.Number || !in.Integer {
		return Value{}, nil, errs.WrongTypeForVariable(d.Name)
	}
	n, ok := new(big.Int).SetString(in.Text, 10)
	if !ok {
		return Value{}, nil, errs.WrongTypeForVariable(d.Name)
	}
	lo, hi := new(big.Int).SetUint64(r.min), new(big.Int).SetUint64(r.max)
	if r.max == 0 {
		hi.SetUint64(1<<64 - 1)
	}
	var warnings []*errs.Error
	switch {
	case n.Cmp(lo) < 0:
		warnings = append(warnings, errs.TruncatedValue(d.Name, in.Text))
		n = lo
	case n.Cmp(hi) > 0:
		warnings = append(warnings, errs.TruncatedValue(d.Name, in.Text))
		n = hi
	}
	return Value{Text: n.String()}, warnings, nil
}

func stringInput(d *Def, r rule, text string) (Value, []*errs.Error, *errs.Error) {
	if len(r.values) > 0 {
		for _, v := range r.values {
			if strings.EqualFold(v, text) {
				return Value{Text: v}, nil, nil
			}
		}
		if d.Name == "transaction_isolation" && strings.EqualFold(text, "READ-UNCOMMITTED") {
			return Value{}, nil, errs.Unsupported("READ UNCOMMITTED", "SQLite never reads uncommitted data")
		}
		return Value{}, nil, errs.WrongValueForVariable(d.Name, text)
	}
	switch d.Name {
	case "sql_mode":
		mode, warnings, e := NormalizeSQLMode(text)
		return Value{Text: mode}, warnings, e
	case "character_set_client", "character_set_connection", "character_set_results",
		"character_set_server", "character_set_database":
		cs, e := LookupCharset(text)
		if e != nil {
			return Value{}, nil, e
		}
		return Value{Text: cs.Name}, nil, nil
	case "collation_connection", "collation_database", "collation_server":
		co, ok := charset.LookupCollation(text)
		if !ok {
			return Value{}, nil, errs.UnknownCollation(text)
		}
		return Value{Text: co.Name}, nil, nil
	case "time_zone":
		return timeZone(text)
	}
	return Value{Text: text}, nil, nil
}

// LookupCharset resolves a charset name for SET: 1115 for an unknown one, 1235 for one
// MySQL has but mysqlite cannot serve without transcoding.
func LookupCharset(name string) (*charset.Charset, *errs.Error) {
	if cs, ok := charset.Lookup(name); ok {
		return cs, nil
	}
	if charset.Known(name) {
		return nil, errs.Unsupported("character set "+strings.ToLower(name), "mysqlite sends and receives UTF-8 only; transcoding arrives later")
	}
	return nil, errs.UnknownCharset(name)
}

// timeZone accepts SYSTEM, UTC and offsets from -13:59 to +14:00. Named zones need
// MySQL's time zone tables; they are 1298, as on a server without them.
func timeZone(text string) (Value, []*errs.Error, *errs.Error) {
	switch strings.ToUpper(text) {
	case "SYSTEM":
		return Value{Text: "SYSTEM"}, nil, nil
	case "UTC":
		return Value{Text: "UTC"}, nil, nil
	}
	if len(text) >= 5 && (text[0] == '+' || text[0] == '-') {
		h, m, ok := strings.Cut(text[1:], ":")
		hh, e1 := strconv.Atoi(h)
		mm, e2 := strconv.Atoi(m)
		if ok && e1 == nil && e2 == nil && len(m) == 2 && mm < 60 {
			minutes := hh*60 + mm
			if (text[0] == '+' && minutes <= 14*60) || (text[0] == '-' && minutes <= 13*60+59) {
				return Value{Text: text}, nil, nil
			}
		}
	}
	return Value{}, nil, errs.UnknownTimeZone(text)
}

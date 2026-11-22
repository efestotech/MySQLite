package vars

// Pair is one SHOW VARIABLES row.
type Pair struct {
	Name, Value string
}

// Show lists what SHOW [GLOBAL | SESSION] VARIABLES reports, sorted by name. GLOBAL
// lists the variables with a global value; SESSION every variable, with its session
// value where it has one. Booleans read as ON/OFF and NULL as the empty string, as
// MySQL prints them there (`oracle: pending`).
func (s *Session) Show(global bool) []Pair {
	scope := ReadDefault
	if global {
		scope = ReadGlobal
	}
	var out []Pair
	for _, name := range Names() {
		d, _ := Lookup(name)
		if global && d.Scope&ScopeGlobal == 0 {
			continue
		}
		v, _, err := s.Get(name, scope)
		if err != nil {
			continue
		}
		text := v.Text
		switch {
		case v.Null:
			text = ""
		case d.Kind == KindBool && text == "1":
			text = "ON"
		case d.Kind == KindBool && text == "0":
			text = "OFF"
		}
		out = append(out, Pair{Name: d.Name, Value: text})
	}
	return out
}

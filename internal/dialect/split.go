package dialect

// Split splits a COM_QUERY text into its statements for CLIENT_MULTI_STATEMENTS: at each
// ';' outside string literals, quoted identifiers and comments. A /*! ... */ comment is
// kept whole. Pieces holding only whitespace and comments are dropped, so a trailing
// ';' or comment adds no statement. With fewer than two statements the query is
// returned unchanged, for the translator to handle as one.
//
// The split is lexical, like MySQL's statement-by-statement parse: a syntax error in a
// later statement is reported when that statement runs, after the earlier ones.
func (t *Translator) Split(q string) []string {
	backslash := !t.mode.HasNoBackslashEscapesMode()
	var out []string
	add := func(s string) {
		if !blank(s) {
			out = append(out, s)
		}
	}
	start, n := 0, len(q)
	for i := 0; i < n; {
		c := q[i]
		switch {
		case c == '\'' || c == '"':
			i = skipQuoted(q, i, c, backslash)
		case c == '`':
			i = skipQuoted(q, i, c, false)
		case c == '#':
			i = skipLine(q, i)
		case isDashComment(q, i):
			i = skipLine(q, i)
		case c == '/' && i+1 < n && q[i+1] == '*':
			i = commentEnd(q, i+2)
		case c == ';':
			add(q[start:i])
			i++
			start = i
		default:
			i++
		}
	}
	add(q[start:])
	if len(out) < 2 {
		return []string{q}
	}
	return out
}

// isDashComment reports a "-- " comment at i: MySQL needs whitespace after the dashes.
func isDashComment(q string, i int) bool {
	n := len(q)
	return q[i] == '-' && i+1 < n && q[i+1] == '-' && (i+2 == n || q[i+2] <= ' ')
}

// blank reports whether s holds only whitespace and ordinary comments. A /*! ... */
// comment is content.
func blank(s string) bool {
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v':
			i++
		case c == '#' || isDashComment(s, i):
			i = skipLine(s, i)
		case c == '/' && i+2 < n && s[i+1] == '*' && s[i+2] == '!':
			return false
		case c == '/' && i+1 < n && s[i+1] == '*':
			i = commentEnd(s, i+2)
		default:
			return false
		}
	}
	return true
}

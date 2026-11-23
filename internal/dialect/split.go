package dialect

// Next returns the first statement of a COM_QUERY text for CLIENT_MULTI_STATEMENTS: the
// text up to the first ';' outside string literals, quoted identifiers and comments,
// with the current sql_mode (NO_BACKSLASH_ESCAPES, ANSI_QUOTES). A /*! ... */ comment
// is kept whole, and pieces holding only whitespace and comments are skipped. rest is
// the text after that ';'; more reports whether rest holds another statement.
//
// The caller runs first before splitting rest, so a statement that changes sql_mode
// affects how the next one is split, as in MySQL's statement-by-statement parse; a
// syntax error in a later statement is reported when that statement runs.
func (t *Translator) Next(q string) (first, rest string, more bool) {
	for {
		piece, after, cut := t.cut(q)
		if !blank(piece) || !cut {
			if !cut {
				return piece, "", false
			}
			return piece, after, !blank(after)
		}
		q = after
	}
}

// Split splits q into its statements with the current sql_mode. With fewer than two
// statements the query is returned unchanged, for the translator to handle as one.
func (t *Translator) Split(q string) []string {
	var out []string
	for rest, more := q, true; more; {
		var first string
		first, rest, more = t.Next(rest)
		if !blank(first) {
			out = append(out, first)
		}
	}
	if len(out) < 2 {
		return []string{q}
	}
	return out
}

// cut splits q at its first top-level ';'.
func (t *Translator) cut(q string) (piece, after string, found bool) {
	backslash := !t.mode.HasNoBackslashEscapesMode()
	ansi := t.mode.HasANSIQuotesMode()
	n := len(q)
	for i := 0; i < n; {
		c := q[i]
		switch {
		case c == '\'':
			i = skipQuoted(q, i, c, backslash)
		case c == '"':
			// An identifier under ANSI_QUOTES: no backslash escapes.
			i = skipQuoted(q, i, c, backslash && !ansi)
		case c == '`':
			i = skipQuoted(q, i, c, false)
		case c == '#', isDashComment(q, i):
			i = skipLine(q, i)
		case c == '/' && i+1 < n && q[i+1] == '*':
			i = commentEnd(q, i+2)
		case c == ';':
			return q[:i], q[i+1:], true
		default:
			i++
		}
	}
	return q, "", false
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

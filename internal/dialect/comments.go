package dialect

// gateVersionComments applies rule R001 before parsing. The TiDB parser executes the
// content of every /*!NNNNN ... */ comment regardless of NNNNN, and the content of
// /*T! ... */ comments, while MySQL skips a versioned comment above its own version and
// treats /*T! as a plain comment. Such comments are replaced by spaces of the same
// length, keeping newlines, so error positions and line numbers stay valid.
//
// The scan skips string literals, quoted identifiers and ordinary comments, so comment
// markers inside them are left alone.
func gateVersionComments(q string, version int, noBackslashEscapes bool) (string, bool) {
	var out []byte
	blank := func(from, to int) {
		if out == nil {
			out = []byte(q)
		}
		for k := from; k < to; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
	}
	n := len(q)
	for i := 0; i < n; {
		c := q[i]
		switch {
		case c == '\'' || c == '"':
			i = skipQuoted(q, i, c, !noBackslashEscapes)
		case c == '`':
			i = skipQuoted(q, i, c, false)
		case c == '#':
			i = skipLine(q, i)
		case c == '-' && i+1 < n && q[i+1] == '-' && (i+2 == n || q[i+2] <= ' '):
			i = skipLine(q, i)
		case c == '/' && i+1 < n && q[i+1] == '*':
			end := commentEnd(q, i+2)
			switch {
			case i+2 < n && q[i+2] == '!':
				j := i + 3
				if v, ok := versionDigits(q, j); ok {
					if v > version {
						blank(i, end)
						i = end
						continue
					}
					j += 5
				}
				// The content is executed: keep scanning it as SQL.
				i = j
			case i+3 < n && q[i+2] == 'T' && q[i+3] == '!':
				blank(i, end)
				i = end
			default:
				i = end
			}
		default:
			i++
		}
	}
	if out == nil {
		return q, false
	}
	return string(out), true
}

// skipQuoted returns the index after the quoted token starting at i. A doubled quote
// is part of the token; a backslash escapes the next byte when backslash is set.
func skipQuoted(q string, i int, quote byte, backslash bool) int {
	for j := i + 1; j < len(q); j++ {
		switch {
		case backslash && q[j] == '\\':
			j++
		case q[j] == quote:
			if j+1 < len(q) && q[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(q)
}

func skipLine(q string, i int) int {
	for j := i; j < len(q); j++ {
		if q[j] == '\n' {
			return j + 1
		}
	}
	return len(q)
}

// commentEnd returns the index after the "*/" that closes a comment whose body starts
// at from, or len(q) when it is unterminated (the parser reports that).
func commentEnd(q string, from int) int {
	for j := from; j+1 < len(q); j++ {
		if q[j] == '*' && q[j+1] == '/' {
			return j + 2
		}
	}
	return len(q)
}

// versionDigits reads the five-digit version of an executable comment.
func versionDigits(q string, j int) (int, bool) {
	if j+5 > len(q) {
		return 0, false
	}
	v := 0
	for k := j; k < j+5; k++ {
		if q[k] < '0' || q[k] > '9' {
			return 0, false
		}
		v = v*10 + int(q[k]-'0')
	}
	return v, true
}

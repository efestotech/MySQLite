package dialect

import (
	"sort"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/test_driver"

	"mysqlite/internal/errs"
)

// Marker is one `?` parameter marker of a prepared statement (ADR 0009).
type Marker struct {
	// Offset is the byte offset of the `?` in the statement text.
	Offset int
	// Limit marks a marker that is a LIMIT or OFFSET count.
	Limit bool
}

// Markers parses a statement to prepare and returns its parameter markers in text
// order. A `?` inside a string, an identifier or a comment is not a marker.
func (t *Translator) Markers(query string) ([]Marker, *errs.Error) {
	gated, _ := gateVersionComments(query, t.cfg.ServerVersion, t.mode.HasNoBackslashEscapesMode())
	stmts, _, err := t.p.ParseSQL(gated)
	if err != nil {
		return nil, parseError(err, gated)
	}
	switch len(stmts) {
	case 0:
		return nil, errs.EmptyQuery()
	case 1:
	default:
		return nil, parseErrorAt(gated, secondStatementOffset(gated, stmts))
	}
	c := &markerCollector{limit: map[*test_driver.ParamMarkerExpr]bool{}}
	stmts[0].Accept(c)
	sort.Slice(c.markers, func(i, j int) bool { return c.markers[i].Offset < c.markers[j].Offset })
	return c.markers, nil
}

type markerCollector struct {
	markers []Marker
	limit   map[*test_driver.ParamMarkerExpr]bool
}

func (c *markerCollector) Enter(n ast.Node) (ast.Node, bool) {
	switch x := n.(type) {
	case *ast.Limit:
		for _, e := range []ast.ExprNode{x.Count, x.Offset} {
			if m, ok := e.(*test_driver.ParamMarkerExpr); ok {
				c.limit[m] = true
			}
		}
	case *test_driver.ParamMarkerExpr:
		c.markers = append(c.markers, Marker{Offset: x.Offset, Limit: c.limit[x]})
	}
	return n, false
}

func (c *markerCollector) Leave(n ast.Node) (ast.Node, bool) { return n, true }

// Substitute writes values[i] in place of the `?` of markers[i].
func Substitute(query string, markers []Marker, values []string) string {
	var b strings.Builder
	last := 0
	for i, m := range markers {
		b.WriteString(query[last:m.Offset])
		b.WriteString(values[i])
		last = m.Offset + 1
	}
	b.WriteString(query[last:])
	return b.String()
}

// Placeholders are the values Substitute writes at PREPARE, when no value is bound:
// NULL, or 0 for a LIMIT count.
func Placeholders(markers []Marker) []string {
	out := make([]string, len(markers))
	for i, m := range markers {
		out[i] = "NULL"
		if m.Limit {
			out[i] = "0"
		}
	}
	return out
}

// NoBackslashEscapes reports the session's NO_BACKSLASH_ESCAPES, which decides how a
// parameter is written as a string literal.
func (t *Translator) NoBackslashEscapes() bool {
	return t.mode.HasNoBackslashEscapesMode()
}

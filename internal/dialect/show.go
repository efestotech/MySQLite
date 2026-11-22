package dialect

import (
	"github.com/pingcap/tidb/pkg/parser/ast"
)

// ShowType is the SHOW statement kind.
type ShowType int

const (
	ShowWarnings ShowType = iota + 1
	ShowErrors
)

// Show describes a SHOW statement; the executor builds its result from session or
// catalog state (docs/04-session-catalog.md section 4).
type Show struct {
	Type ShowType
	// Count is SHOW COUNT(*) WARNINGS / ERRORS.
	Count bool
}

func (e *emitter) show(n *ast.ShowStmt) *Statement {
	switch n.Tp {
	case ast.ShowWarnings, ast.ShowErrors:
		t := ShowWarnings
		if n.Tp == ast.ShowErrors {
			t = ShowErrors
		}
		return &Statement{Kind: KindShow, Show: &Show{Type: t, Count: n.CountWarningsOrErrors}}
	}
	e.unsupported(statementName(n), reasonLater)
	return nil
}

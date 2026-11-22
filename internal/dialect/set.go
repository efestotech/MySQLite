package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/meta"
)

// AssignKind is the form of one SET assignment.
type AssignKind int

const (
	// AssignVariable is [GLOBAL|SESSION] name = value, @@[scope.]name = value.
	AssignVariable AssignKind = iota + 1
	// AssignNames is SET NAMES charset [COLLATE collation] | DEFAULT.
	AssignNames
	// AssignCharset is SET CHARACTER SET charset | DEFAULT.
	AssignCharset
)

// Assignment is one SET assignment. The executor validates and applies the whole list
// (docs/04-session-catalog.md section 1).
type Assignment struct {
	Kind AssignKind
	// Name is the variable, lower case. SET TRANSACTION assigns transaction_isolation
	// or transaction_read_only.
	Name   string
	Global bool
	// OneShot is SET TRANSACTION without a scope: it applies to the next transaction only.
	OneShot bool
	Value   SetValue
	// Collation is the COLLATE of SET NAMES, or "".
	Collation string
}

// SetValue is the right-hand side of an assignment. Only constants, DEFAULT, bare words
// (SET autocommit = OFF) and other system variables are accepted; expressions arrive
// with M3.
type SetValue struct {
	Default bool
	// Literal is a constant: NULL, number or string.
	Literal *meta.Literal
	// Word is a bare identifier used as a value, e.g. OFF.
	Word string
	// Var is another system variable, read before the assignment.
	Var *Variable
}

// String is the value as MySQL quotes it in error 1231.
func (v SetValue) String() string {
	switch {
	case v.Default:
		return "DEFAULT"
	case v.Literal != nil:
		return v.Literal.Text
	case v.Var != nil:
		return "@@" + v.Var.Name
	}
	return v.Word
}

// TiDB parses SET NAMES and SET CHARACTER SET as these pseudo-variables, and
// SET [scope] TRANSACTION ... as tx_isolation / tx_read_only (with a _one_shot suffix
// when no scope is given).
const (
	tidbSetNames   = "SetNAMES"
	tidbSetCharset = "SetCharset"
)

func (e *emitter) set(n *ast.SetStmt) *Statement {
	// SET TRANSACTION without GLOBAL or SESSION applies to the next transaction only.
	// The parser marks that with a _one_shot suffix for the isolation level but not for
	// the access mode, so the words decide.
	transaction, oneShot := false, false
	ws := words(e.query)
	for i, w := range ws {
		if strings.EqualFold(w.text, "TRANSACTION") {
			transaction = true
			oneShot = i > 0 && strings.EqualFold(ws[i-1].text, "SET")
		}
	}
	var list []Assignment
	for _, v := range n.Variables {
		a := Assignment{Global: v.IsGlobal}
		switch {
		case v.Name == tidbSetNames || v.Name == tidbSetCharset:
			a.Kind = AssignNames
			if v.Name == tidbSetCharset {
				a.Kind = AssignCharset
			}
			if v.ExtendValue != nil {
				if s, ok := v.ExtendValue.GetValue().(string); ok {
					a.Collation = strings.ToLower(s)
				}
			}
		case !v.IsSystem:
			e.unsupported("user variable", reasonLater)
			return nil
		case transaction && strings.HasPrefix(v.Name, "tx_isolation"):
			a.Kind, a.Name, a.OneShot = AssignVariable, "transaction_isolation", oneShot
		case transaction && strings.HasPrefix(v.Name, "tx_read_only"):
			a.Kind, a.Name, a.OneShot = AssignVariable, "transaction_read_only", oneShot
		default:
			a.Kind, a.Name = AssignVariable, strings.ToLower(v.Name)
		}
		val, ok := setValue(v.Value)
		if !ok {
			e.unsupported("expression in SET", reasonLater)
			return nil
		}
		if transaction && a.Name == "transaction_read_only" && val.Literal != nil {
			// The parser gives READ ONLY / READ WRITE as the string '1' / '0'.
			val.Literal = &meta.Literal{Kind: meta.LiteralInt, Text: val.Literal.Text}
		}
		a.Value = val
		list = append(list, a)
	}
	return &Statement{Kind: KindSet, Set: list}
}

func setValue(x ast.ExprNode) (SetValue, bool) {
	switch n := x.(type) {
	case *ast.DefaultExpr:
		return SetValue{Default: n.Name == nil}, n.Name == nil
	case *ast.ColumnNameExpr:
		if n.Name.Table.O != "" || n.Name.Schema.O != "" {
			return SetValue{}, false
		}
		return SetValue{Word: n.Name.Name.O}, true
	case *ast.VariableExpr:
		if !n.IsSystem {
			return SetValue{}, false
		}
		return SetValue{Var: &Variable{Name: n.Name, Scope: variableScope(n)}}, true
	}
	if lit := valueLiteral(x); lit != nil {
		return SetValue{Literal: lit}, true
	}
	return SetValue{}, false
}

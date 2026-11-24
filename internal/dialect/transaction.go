package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
)

// TxOp is a transaction control statement.
type TxOp int

const (
	// TxBegin is BEGIN or START TRANSACTION. An open transaction is committed first.
	TxBegin TxOp = iota + 1
	// TxCommit is COMMIT; without a transaction it does nothing.
	TxCommit
	// TxRollback is ROLLBACK; without a transaction it does nothing.
	TxRollback
	// TxLockTables is LOCK TABLES: it commits and takes no lock (decision Q7).
	TxLockTables
	// TxUnlockTables is UNLOCK TABLES: it commits after a LOCK TABLES of the session.
	TxUnlockTables
)

// Transaction describes a KindTransaction statement (docs/06-transactions.md section 4).
type Transaction struct {
	Op TxOp
	// ReadOnly is START TRANSACTION READ ONLY, ReadWrite is READ WRITE. Without either
	// the transaction follows transaction_read_only.
	ReadOnly, ReadWrite bool
	// Snapshot is WITH CONSISTENT SNAPSHOT: the snapshot starts with the statement, not
	// with the first read.
	Snapshot bool
	// Tables are LOCK TABLES' tables, database and name: they must exist (1146), which
	// MySQL checks after the implicit commit.
	Tables [][2]string
}

func (e *emitter) begin(n *ast.BeginStmt) *Statement {
	// TiDB accepts extensions that are syntax errors in MySQL.
	switch {
	case n.Mode != "":
		e.fail(e.parseErrorAtWord(n.Mode))
		return nil
	case n.CausalConsistencyOnly:
		e.fail(e.parseErrorAtWord("CAUSAL"))
		return nil
	case n.AsOf != nil:
		e.fail(e.parseErrorAtWord("AS"))
		return nil
	}
	// The parser builds the same node for START TRANSACTION, READ WRITE and WITH
	// CONSISTENT SNAPSHOT. The grammar of the statement has no strings or identifiers,
	// so the SNAPSHOT keyword outside comments identifies the clause.
	tx := &Transaction{Op: TxBegin, ReadOnly: n.ReadOnly}
	for _, w := range words(e.query) {
		switch {
		case strings.EqualFold(w.text, "SNAPSHOT"):
			tx.Snapshot = true
		case strings.EqualFold(w.text, "WRITE"):
			tx.ReadWrite = true
		}
	}
	return &Statement{Kind: KindTransaction, Tx: tx}
}

func (e *emitter) completion(op TxOp, c ast.CompletionType) *Statement {
	name := "COMMIT"
	if op == TxRollback {
		name = "ROLLBACK"
	}
	switch c {
	case ast.CompletionTypeChain:
		e.unsupported(name+" AND CHAIN", reasonLater)
		return nil
	case ast.CompletionTypeRelease:
		e.unsupported(name+" RELEASE", reasonLater)
		return nil
	}
	return &Statement{Kind: KindTransaction, Tx: &Transaction{Op: op}}
}

// parseErrorAtWord is a syntax error at the first occurrence of the keyword.
func (e *emitter) parseErrorAtWord(keyword string) *errs.Error {
	for _, w := range words(e.query) {
		if strings.EqualFold(w.text, keyword) {
			return parseErrorAt(e.query, w.offset)
		}
	}
	return parseErrorAt(e.query, 0)
}

type word struct {
	text   string
	offset int
}

// words returns the keywords of a statement outside string literals, quoted
// identifiers and comments. The content of /*! ... */ comments counts: after R001,
// only the ones MySQL executes remain.
func words(q string) []word {
	var out []word
	n := len(q)
	for i := 0; i < n; {
		c := q[i]
		switch {
		case c == '\'' || c == '"':
			i = skipQuoted(q, i, c, true)
		case c == '`':
			i = skipQuoted(q, i, c, false)
		case c == '#':
			i = skipLine(q, i)
		case c == '-' && i+1 < n && q[i+1] == '-' && (i+2 == n || q[i+2] <= ' '):
			i = skipLine(q, i)
		case c == '/' && i+2 < n && q[i+1] == '*' && q[i+2] == '!':
			i += 3
			if _, ok := versionDigits(q, i); ok {
				i += 5
			}
		case c == '/' && i+1 < n && q[i+1] == '*':
			i = commentEnd(q, i+2)
		case isWordByte(c):
			j := i
			for j < n && isWordByte(q[j]) {
				j++
			}
			out = append(out, word{q[i:j], i})
			i = j
		default:
			i++
		}
	}
	return out
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

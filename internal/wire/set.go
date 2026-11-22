package wire

import (
	"context"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/charset"
	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
	"mysqlite/internal/vars"
)

// set runs SET (docs/04-session-catalog.md section 1). Every assignment is checked
// before any is applied, so a failing SET changes nothing, as in MySQL.
func (h *handler) set(list []dialect.Assignment) (*mysql.Result, error) {
	type step struct {
		change  vars.Change
		oneShot bool
	}
	var steps []step
	check := func(name string, global bool, in vars.Input, oneShot bool) *errs.Error {
		c, warnings, e := h.vars.Check(name, global, in)
		if e != nil {
			return e
		}
		for _, w := range warnings {
			h.warn(w)
		}
		steps = append(steps, step{change: c, oneShot: oneShot})
		return nil
	}
	for _, a := range list {
		var e *errs.Error
		switch a.Kind {
		case dialect.AssignVariable:
			var in vars.Input
			if in, e = h.setInput(a.Value); e == nil {
				if a.OneShot && h.sess.InTransaction() {
					e = errs.TransactionInProgress()
				} else {
					e = check(a.Name, a.Global, in, a.OneShot)
				}
			}
		case dialect.AssignNames, dialect.AssignCharset:
			e = h.checkNames(a, check)
		}
		if e != nil {
			h.pendingDiag = nil
			return nil, toMySQL(e)
		}
	}
	ctx := context.Background()
	for _, s := range steps {
		c := s.change
		if s.oneShot {
			// SET TRANSACTION without a scope: transaction_isolation has no effect on
			// SQLite's snapshot transactions, so only the access mode is kept.
			if c.Def.Name == "transaction_read_only" {
				h.sess.SetNextReadOnly(c.Value.Text == "1")
			}
			continue
		}
		h.vars.Apply(c)
		if c.Global {
			continue
		}
		switch c.Def.Name {
		case "autocommit":
			if err := h.sess.SetAutocommit(ctx, c.Value.Text == "1"); err != nil {
				return nil, h.fail(err, errs.Hint{})
			}
		case "transaction_read_only":
			h.sess.SetReadOnly(c.Value.Text == "1")
		case "sql_mode":
			h.tr.SetSQLMode(c.Value.Text)
		}
	}
	return &mysql.Result{}, nil
}

// checkNames checks SET NAMES and SET CHARACTER SET, which set several variables.
func (h *handler) checkNames(a dialect.Assignment, check func(string, bool, vars.Input, bool) *errs.Error) *errs.Error {
	name := "utf8mb4"
	if !a.Value.Default {
		in, e := h.setInput(a.Value)
		if e != nil {
			return e
		}
		if in.Null || in.Number {
			return errs.UnknownCharset(a.Value.String())
		}
		name = in.Text
	}
	cs, e := vars.LookupCharset(name)
	if e != nil {
		return e
	}
	text := func(s string) vars.Input { return vars.Input{Text: s} }
	if a.Kind == dialect.AssignCharset {
		// SET CHARACTER SET: the connection takes the database's character set.
		db, _, e := h.vars.Get("collation_database", vars.ReadSession)
		if e != nil {
			return e
		}
		for _, n := range []string{"character_set_client", "character_set_results"} {
			if e := check(n, false, text(cs.Name), false); e != nil {
				return e
			}
		}
		return check("collation_connection", false, text(db.Text), false)
	}
	collation := cs.DefaultCollation
	if a.Collation != "" {
		co, ok := charset.LookupCollation(a.Collation)
		if !ok {
			return errs.UnknownCollation(a.Collation)
		}
		if co.Charset != cs.Name {
			return errs.CollationCharsetMismatch(co.Name, cs.Name)
		}
		collation = co.Name
	}
	for _, n := range []string{"character_set_client", "character_set_results"} {
		if e := check(n, false, text(cs.Name), false); e != nil {
			return e
		}
	}
	return check("collation_connection", false, text(collation), false)
}

// setInput turns the right-hand side of an assignment into a vars.Input. Another
// system variable is read now, before any assignment is applied.
func (h *handler) setInput(v dialect.SetValue) (vars.Input, *errs.Error) {
	switch {
	case v.Default:
		return vars.Input{Default: true}, nil
	case v.Var != nil:
		val, def, e := h.vars.Get(v.Var.Name, v.Var.Scope)
		if e != nil {
			return vars.Input{}, e
		}
		if val.Null {
			return vars.Input{Null: true}, nil
		}
		switch def.Kind {
		case vars.KindInt, vars.KindUint, vars.KindBool:
			return vars.Input{Number: true, Integer: true, Text: val.Text}, nil
		case vars.KindDouble:
			return vars.Input{Number: true, Text: val.Text}, nil
		}
		return vars.Input{Text: val.Text}, nil
	case v.Literal != nil:
		switch v.Literal.Kind {
		case meta.LiteralNull:
			return vars.Input{Null: true}, nil
		case meta.LiteralInt:
			return vars.Input{Number: true, Integer: true, Text: v.Literal.Text}, nil
		case meta.LiteralDecimal, meta.LiteralFloat:
			return vars.Input{Number: true, Text: v.Literal.Text}, nil
		}
		return vars.Input{Text: v.Literal.Text}, nil
	}
	return vars.Input{Text: v.Word}, nil
}

// applyGlobals starts a connection with the global values of the variables that change
// how statements run, as SET GLOBAL leaves them.
func (h *handler) applyGlobals() {
	if v, _, e := h.vars.Get("autocommit", vars.ReadGlobal); e == nil && v.Text == "0" {
		_ = h.sess.SetAutocommit(context.Background(), false)
	}
	if v, _, e := h.vars.Get("transaction_read_only", vars.ReadGlobal); e == nil {
		h.sess.SetReadOnly(v.Text == "1")
	}
	if v, _, e := h.vars.Get("sql_mode", vars.ReadSession); e == nil && v.Text != vars.DefaultSQLMode {
		h.tr.SetSQLMode(v.Text)
	}
}

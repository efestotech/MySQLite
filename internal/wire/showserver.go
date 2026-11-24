package wire

import (
	"strconv"
	"time"

	"mysqlite/internal/charset"
	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/infoschema"
	"mysqlite/internal/meta"
)

// Server SHOW statements (docs/04-session-catalog.md section 4): rows built from the
// variable registry, the charset table, the process list and the configured users.
// Formats and lengths are `oracle: pending`.

func (h *handler) showServer(s *dialect.Show) ([]meta.ColumnDef, [][]any, error) {
	switch s.Type {
	case dialect.ShowVariables:
		defs := []meta.ColumnDef{textCol("Variable_name", 64), textCol("Value", 1024)}
		var rows [][]any
		for _, p := range h.vars.Show(s.Global) {
			rows = append(rows, []any{p.Name, p.Value})
		}
		return defs, rows, nil
	case dialect.ShowStatus:
		return []meta.ColumnDef{textCol("Variable_name", 64), textCol("Value", 1024)}, h.statusRows(s), nil
	case dialect.ShowEngines:
		defs := []meta.ColumnDef{
			textCol("Engine", 64), textCol("Support", 8), textCol("Comment", 80),
			textCol("Transactions", 3), textCol("XA", 3), textCol("Savepoints", 3),
		}
		return defs, [][]any{{"SQLite", "DEFAULT", "SQLite storage through mysqlite", "YES", "NO", "NO"}}, nil
	case dialect.ShowCharset:
		defs := []meta.ColumnDef{textCol("Charset", 64), textCol("Description", 2048), textCol("Default collation", 64), longCol("Maxlen")}
		var rows [][]any
		for _, c := range charset.Charsets {
			rows = append(rows, []any{c.Name, c.Description, c.DefaultCollation, int64(c.MaxLen)})
		}
		return defs, rows, nil
	case dialect.ShowCollation:
		defs := []meta.ColumnDef{
			textCol("Collation", 64), textCol("Charset", 64), numCol("Id"), textCol("Default", 3),
			textCol("Compiled", 3), numCol("Sortlen"), textCol("Pad_attribute", 9),
		}
		var rows [][]any
		for _, c := range charset.Collations {
			def := ""
			if c.Default {
				def = "Yes"
			}
			rows = append(rows, []any{c.Name, c.Charset, int64(c.ID), def, "Yes", infoschema.SortLen(c.Name), c.PadAttribute})
		}
		return defs, rows, nil
	case dialect.ShowProcessList:
		return processDefs(s), h.processRows(s), nil
	}
	return h.showGrants(s)
}

func (h *handler) statusRows(s *dialect.Show) [][]any {
	var p *process
	if !s.Global {
		p = h.proc
	}
	c := h.procs.status(p)
	itoa := func(n int64) string { return strconv.FormatInt(n, 10) }
	return [][]any{
		{"Aborted_clients", "0"},
		{"Aborted_connects", "0"},
		{"Connections", itoa(c.connections)},
		{"Max_used_connections", itoa(int64(c.maxUsed))},
		{"Queries", itoa(c.questions)},
		{"Questions", itoa(c.questions)},
		{"Ssl_cipher", ""},
		{"Ssl_version", ""},
		{"Threads_connected", itoa(int64(c.connected))},
		{"Threads_running", itoa(int64(c.running))},
		{"Uptime", itoa(c.uptime)},
		{"Uptime_since_flush_status", itoa(c.uptime)},
	}
}

func processDefs(s *dialect.Show) []meta.ColumnDef {
	infoLen := 100
	if s.Full {
		infoLen = 65535
	}
	// MySQL 8.0.46's column definitions (oracle, --column-type-info).
	notNull := func(d meta.ColumnDef) meta.ColumnDef {
		d.Flags |= meta.FlagNotNull
		return d
	}
	return []meta.ColumnDef{
		{Name: "Id", Charset: meta.CharsetBinary, Type: meta.TypeLongLong, Flags: meta.FlagNotNull | meta.FlagBinary | meta.FlagNum, Length: 22},
		notNull(textCol("User", 32)), notNull(textCol("Host", 255)), textCol("db", 64),
		notNull(textCol("Command", 16)),
		{Name: "Time", Charset: meta.CharsetBinary, Type: meta.TypeLong, Flags: meta.FlagNotNull | meta.FlagBinary | meta.FlagNum, Length: 8},
		textCol("State", 30), textCol("Info", infoLen),
	}
}

func (h *handler) processRows(s *dialect.Show) [][]any {
	var rows [][]any
	now := time.Now()
	for _, p := range h.procs.snapshot() {
		var db, info, state any
		if p.DB != "" {
			db = p.DB
		}
		if p.Command == "Query" {
			text := p.Info
			if !s.Full {
				if r := []rune(text); len(r) > 100 {
					text = string(r[:100])
				}
			}
			info, state = text, "executing"
			if h.proc != nil && p.ID == h.proc.id {
				state = "init" // the session running SHOW PROCESSLIST, as MySQL shows it
			}
		}
		rows = append(rows, []any{int64(p.ID), p.User, p.Host, db, p.Command, int64(now.Sub(p.Since).Seconds()), state, info})
	}
	return rows
}

// showGrants reports that every configured user has every privilege: mysqlite has no
// privilege system (docs/00-brief.md section 6).
func (h *handler) showGrants(s *dialect.Show) ([]meta.ColumnDef, [][]any, error) {
	user, host := h.user(), "%"
	if s.User != nil {
		user, host = s.User.Name, s.User.Host
		if _, ok := h.users[user]; !ok || host != "%" {
			return nil, nil, toMySQL(errs.NoSuchGrant(user, host))
		}
	}
	name := "Grants for " + user + "@" + host
	grant := "GRANT ALL PRIVILEGES ON *.* TO `" + user + "`@`" + host + "` WITH GRANT OPTION"
	return []meta.ColumnDef{textCol(name, 1024)}, [][]any{{grant}}, nil
}

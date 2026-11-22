package wire

import (
	"sort"
	"sync"
	"time"
)

// processList tracks the open connections for SHOW PROCESSLIST and the server status
// counters for SHOW STATUS. It is shared by every connection.
type processList struct {
	started time.Time

	mu          sync.Mutex
	procs       map[uint32]*process
	connections int64 // authenticated connections since start
	maxUsed     int
	questions   int64
}

func newProcessList() *processList {
	return &processList{started: time.Now(), procs: make(map[uint32]*process)}
}

// process is one connection's row. Its fields change under processList.mu.
type process struct {
	id   uint32
	user string
	host string

	db      string
	command string // "Sleep" or "Query"
	since   time.Time
	info    string
	// questions counts this connection's statements (session Questions).
	questions int64
}

// processSnapshot is a copy of a process taken under the lock.
type processSnapshot struct {
	ID                      uint32
	User, Host, DB, Command string
	Since                   time.Time
	Info                    string
}

func (l *processList) add(id uint32, user, host, db string) *process {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := &process{id: id, user: user, host: host, db: db, command: "Sleep", since: time.Now()}
	l.procs[id] = p
	l.connections++
	l.maxUsed = max(l.maxUsed, len(l.procs))
	return p
}

func (l *processList) remove(p *process) {
	if l == nil || p == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.procs, p.id)
}

// begin marks p as running query.
func (l *processList) begin(p *process, db, query string) {
	if l == nil || p == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p.db, p.command, p.info, p.since = db, "Query", query, time.Now()
	p.questions++
	l.questions++
}

// end marks p as idle in database db.
func (l *processList) end(p *process, db string) {
	if l == nil || p == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p.db, p.command, p.info, p.since = db, "Sleep", "", time.Now()
}

// snapshot lists the processes by id.
func (l *processList) snapshot() []processSnapshot {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]processSnapshot, 0, len(l.procs))
	for _, p := range l.procs {
		out = append(out, processSnapshot{ID: p.id, User: p.user, Host: p.host, DB: p.db, Command: p.command, Since: p.since, Info: p.info})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// statusCounters are the SHOW STATUS values: global, or with the session values of p.
type statusCounters struct {
	connections, questions int64
	connected, running     int
	maxUsed                int
	uptime                 int64
}

func (l *processList) status(p *process) statusCounters {
	if l == nil {
		return statusCounters{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c := statusCounters{
		connections: l.connections, questions: l.questions, connected: len(l.procs),
		maxUsed: l.maxUsed, uptime: int64(time.Since(l.started).Seconds()),
	}
	for _, q := range l.procs {
		if q.command == "Query" {
			c.running++
		}
	}
	if p != nil {
		c.questions = p.questions
	}
	return c
}

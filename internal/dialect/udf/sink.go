package udf

import (
	"math"
	"sync"
	"sync/atomic"
)

// SQL functions are registered once per engine, not per session, so a function that
// must hand a value back to the statement's session (LAST_INSERT_ID(expr)) writes to a
// sink the executor opens for that statement and binds as an argument.

var (
	sinks    sync.Map // int64 -> *sink
	sinkSeq  atomic.Int64
	sinkNone = errSink("udf: LAST_INSERT_ID sink is not open")
)

type errSink string

func (e errSink) Error() string { return string(e) }

type sink struct {
	mu    sync.Mutex
	value int64
	set   bool
}

// NewSink opens a sink for one statement. take returns the last value written, if any,
// and closes the sink.
func NewSink() (id int64, take func() (int64, bool)) {
	id = sinkSeq.Add(1)
	s := &sink{}
	sinks.Store(id, s)
	return id, func() (int64, bool) {
		sinks.Delete(id)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.value, s.set
	}
}

// lastInsertID(sink, x) is LAST_INSERT_ID(x): x as an integer (rounded, as MySQL's
// val_int), written to the sink. LAST_INSERT_ID(NULL) is NULL and sets 0.
func lastInsertID(args []any) (any, error) {
	id, _ := args[0].(int64)
	v, ok := sinks.Load(id)
	if !ok {
		return nil, sinkNone
	}
	var n int64
	var result any
	switch x := number(args[1]).(type) {
	case int64:
		n, result = x, x
	case float64:
		n = int64(math.Round(x))
		result = n
	}
	s := v.(*sink)
	s.mu.Lock()
	s.value, s.set = n, true
	s.mu.Unlock()
	return result, nil
}

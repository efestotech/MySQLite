package engine

import (
	"context"
	"errors"
	"sync"
)

// ErrWriteQueueFull is returned by Writer when write_queue_depth sessions already wait
// for the writer (docs/06-transactions.md section 6); MySQL clients get 1205.
var ErrWriteQueueFull = errors.New("engine: write queue full")

// DefaultWriteQueueDepth is the write_queue_depth default.
const DefaultWriteQueueDepth = 256

// writerQueue orders the sessions waiting for a database's writer. One ticket at a time
// is the head: only the head waits inside database/sql's pool, whose waiters are not
// served in order, and it hands the ticket on once it holds the connection. So the
// writer goes to sessions in arrival order, and at most depth sessions wait.
type writerQueue struct {
	depth int

	mu      sync.Mutex
	head    bool            // a ticket holder is waiting in the pool
	queued  []chan struct{} // waiting for the ticket, in arrival order
	pending int             // the head plus queued
}

func newWriterQueue(depth int) *writerQueue {
	if depth < 1 {
		depth = DefaultWriteQueueDepth
	}
	return &writerQueue{depth: depth}
}

// enter waits for the ticket; the caller then takes the writer and calls leave.
func (q *writerQueue) enter(ctx context.Context) error {
	q.mu.Lock()
	if q.pending >= q.depth {
		q.mu.Unlock()
		return ErrWriteQueueFull
	}
	q.pending++
	if !q.head {
		q.head = true
		q.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	q.queued = append(q.queued, ch)
	q.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
	}
	q.mu.Lock()
	for i, c := range q.queued {
		if c == ch {
			q.queued = append(q.queued[:i], q.queued[i+1:]...)
			q.pending--
			q.mu.Unlock()
			return ctx.Err()
		}
	}
	q.mu.Unlock()
	// The ticket arrived while the context ended: pass it on.
	q.leave()
	return ctx.Err()
}

// leave gives the ticket to the next session in line.
func (q *writerQueue) leave() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending--
	if len(q.queued) == 0 {
		q.head = false
		return
	}
	next := q.queued[0]
	q.queued = q.queued[1:]
	close(next)
}

// waiting is the number of sessions waiting for the writer.
func (q *writerQueue) waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending
}

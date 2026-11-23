package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func queueDB(t *testing.T, depth int) *Database {
	t.Helper()
	opts := testOptions(t)
	opts.WriteQueueDepth = depth
	e := openEngine(t, opts)
	if err := e.CreateDatabase("shop"); err != nil {
		t.Fatal(err)
	}
	db, err := e.Database("shop")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// waitQueued waits until n sessions wait for the writer.
func waitQueued(t *testing.T, db *Database, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for db.queue.waiting() != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting, want %d", db.queue.waiting(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// docs/06-transactions.md section 6: sessions get the writer in arrival order.
func TestWriterQueueIsFIFO(t *testing.T) {
	db := queueDB(t, 16)
	ctx := context.Background()
	holder, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := db.Writer(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			_ = w.Close()
		}(i)
		waitQueued(t, db, i+1) // arrival order is i
	}
	_ = holder.Close()
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("order %v", order)
		}
	}
}

// A full queue is refused at once, not a hang; a waiter whose context ends leaves the
// queue and the others keep their turn.
func TestWriterQueueDepth(t *testing.T) {
	db := queueDB(t, 2)
	ctx := context.Background()
	holder, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 2)
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	go func() { // gives up after 200 ms
		w, err := db.Writer(short)
		if err == nil {
			_ = w.Close()
		}
		got <- err
	}()
	waitQueued(t, db, 1)
	go func() { // waits for its turn
		w, err := db.Writer(ctx)
		if err == nil {
			_ = w.Close()
		}
		got <- err
	}()
	waitQueued(t, db, 2)

	start := time.Now()
	if _, err := db.Writer(ctx); !errors.Is(err, ErrWriteQueueFull) {
		t.Fatalf("third waiter: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("a full queue must not wait (%v)", time.Since(start))
	}
	if err := <-got; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter with a deadline: %v", err)
	}
	waitQueued(t, db, 1)
	_ = holder.Close()
	if err := <-got; err != nil {
		t.Fatalf("waiter after the holder: %v", err)
	}
	waitQueued(t, db, 0)
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
}

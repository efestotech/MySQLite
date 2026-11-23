package engine

import (
	"context"
	"database/sql"
	"math/rand/v2"
	"time"
)

// SQLite primary result codes for a lock held by another connection or process.
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// busyAttempts bounds the attempts to take a database's write lock; each attempt
// already waits busy_timeout inside SQLite (docs/06-transactions.md section 5).
const busyAttempts = 5

// busyBackoff is the first pause between attempts; it doubles after each one.
var busyBackoff = 10 * time.Millisecond

// IsBusy reports whether err is SQLITE_BUSY or SQLITE_LOCKED.
func IsBusy(err error) bool {
	code, _, ok := ErrorCode(err)
	return ok && (code&0xff == sqliteBusy || code&0xff == sqliteLocked)
}

// BeginImmediate starts a write transaction on c, retrying SQLITE_BUSY and
// SQLITE_LOCKED with jittered backoff. Retrying is safe: a BEGIN IMMEDIATE that fails
// leaves nothing behind. It returns the retries made and the last error.
func BeginImmediate(ctx context.Context, c *sql.Conn) (retries int, err error) {
	pause := busyBackoff
	for {
		_, err = c.ExecContext(ctx, "BEGIN IMMEDIATE")
		if err == nil || !IsBusy(err) || retries+1 >= busyAttempts {
			return retries, err
		}
		retries++
		t := time.NewTimer(pause/2 + rand.N(pause))
		select {
		case <-ctx.Done():
			t.Stop()
			return retries, ctx.Err()
		case <-t.C:
		}
		pause *= 2
	}
}

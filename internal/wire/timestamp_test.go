package wire

import (
	"testing"
)

// TIMESTAMP is stored in UTC and shown in the session time zone; DATETIME is never
// converted (ADR 0007). `oracle: pending`.
func TestTimestampTimeZone(t *testing.T) {
	conn := shopServer(t, "CREATE TABLE events (id INT NOT NULL PRIMARY KEY, ts TIMESTAMP NULL, dt DATETIME NULL)")
	steps := []struct {
		query, rows string
	}{
		{"SET time_zone = '+02:00'", ""},
		{"INSERT INTO events VALUES (1, '2024-03-01 12:00:00', '2024-03-01 12:00:00')", ""},
		{"SELECT ts, dt FROM events", "2024-03-01 12:00:00,2024-03-01 12:00:00"},
		{"SET time_zone = '+00:00'", ""},
		{"SELECT ts, dt FROM events", "2024-03-01 10:00:00,2024-03-01 12:00:00"},
		{"SET time_zone = '-05:00'", ""},
		{"SELECT * FROM events", "1,2024-03-01 05:00:00,2024-03-01 12:00:00"},
		{"SELECT e.* FROM events e", "1,2024-03-01 05:00:00,2024-03-01 12:00:00"},
		{"SET time_zone = '+02:00'", ""},
		{"SELECT id FROM events WHERE ts = '2024-03-01 12:00:00'", "1"},
		{"SELECT e.id FROM events e WHERE e.ts > '2024-03-01 11:59:59' ORDER BY e.ts", "1"},
		{"SELECT id FROM events e WHERE EXISTS (SELECT 1 FROM items WHERE e.ts = '2024-03-01 12:00:00')", "1"},
		{"UPDATE events SET ts = '2024-03-01 13:00:00' WHERE id = 1", ""},
		{"SET time_zone = '+00:00'", ""},
		{"SELECT ts FROM events WHERE id = 1", "2024-03-01 11:00:00"},
		{"SET timestamp = 1700000000", ""}, // 2023-11-14 22:13:20 UTC
		{"SET time_zone = '+05:30'", ""},
		{"INSERT INTO events (id, ts, dt) VALUES (2, NOW(), NOW()), (3, NULL, NULL)", ""},
		{"SET time_zone = '+00:00'", ""},
		{"SELECT ts, dt FROM events WHERE id = 2", "2023-11-14 22:13:20,2023-11-15 03:43:20"},
		{"SELECT ts FROM events WHERE id = 3", "NULL"},
	}
	for _, s := range steps {
		res, err := conn.Execute(s.query)
		if err != nil {
			t.Fatalf("%s: %v", s.query, err)
		}
		if s.rows == "" {
			continue
		}
		if got := rowsOf(t, res); got != s.rows {
			t.Errorf("%s: rows %q, want %q", s.query, got, s.rows)
		}
	}

	// UPDATE counts changed rows on the stored value: the same time is no change.
	execOK(t, conn, "SET time_zone = '+02:00'")
	res := execOK(t, conn, "UPDATE events SET ts = '2024-03-01 13:00:00' WHERE id = 1")
	if res.AffectedRows != 0 {
		t.Errorf("same TIMESTAMP: %d affected rows, want 0", res.AffectedRows)
	}
	res = execOK(t, conn, "UPDATE events SET ts = ts WHERE id = 1")
	if res.AffectedRows != 0 {
		t.Errorf("ts = ts: %d affected rows, want 0", res.AffectedRows)
	}
}

func TestTimestampLiteralDefault(t *testing.T) {
	conn := shopServer(t)
	_, err := conn.Execute("CREATE TABLE d (ts TIMESTAMP NULL DEFAULT '2024-01-01 10:00:00')")
	const want = "mysqlite: unsupported construct literal DEFAULT for a TIMESTAMP column: TIMESTAMP is stored in UTC and the default is not converted yet"
	if e := requireMyError(t, err, 1235, "42000"); e.Message != want {
		t.Errorf("got %q", e.Message)
	}
	execOK(t, conn, "CREATE TABLE z (ts TIMESTAMP NULL DEFAULT '0000-00-00 00:00:00', dt DATETIME DEFAULT '2024-01-01 10:00:00')")
}

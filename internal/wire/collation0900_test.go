package wire

import (
	"strings"
	"testing"
)

// utf8mb4_0900_ai_ci and utf8mb4_0900_bin columns: accent- and case-insensitive, NO
// PAD; Italian words. `oracle: pending`.
func TestCollation0900(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE words (w VARCHAR(20) NOT NULL, b VARCHAR(20) COLLATE utf8mb4_0900_bin, UNIQUE KEY uw (w)) COLLATE utf8mb4_0900_ai_ci")
	execOK(t, conn, "INSERT INTO words VALUES ('città', 'città'), ('perché', 'a'), ('Così', 'A')")

	_, err := conn.Execute("INSERT INTO words (w) VALUES ('CITTA')")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry 'CITTA' for key 'words.uw'" {
		t.Errorf("1062 message %q", e.Message)
	}
	// NO PAD: a trailing space makes another value.
	execOK(t, conn, "INSERT INTO words (w) VALUES ('citta ')")

	for q, want := range map[string]string{
		"SELECT w FROM words WHERE w = 'PERCHE'":                    "perché",
		"SELECT w FROM words WHERE w = 'cosi'":                      "Così",
		"SELECT count(*) FROM words WHERE w = 'citta'":              "1",
		"SELECT w FROM words WHERE w LIKE 'CITT_' ORDER BY w":       "città",
		"SELECT w FROM words WHERE w LIKE 'perch%'":                 "perché",
		"SELECT b FROM words WHERE b = 'a'":                         "a",
		"SELECT count(*) FROM words WHERE b = 'a '":                 "0",
		"SELECT count(*) FROM words WHERE b LIKE 'citta'":           "0",
		"SELECT w FROM words WHERE w IN ('PERCHÉ', 'x') ORDER BY w": "perché",
	} {
		if got := rowsOf(t, execOK(t, conn, q)); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
	got := showCreate(t, conn, "words")
	for _, part := range []string{
		"`w` varchar(20) NOT NULL,",
		"`b` varchar(20) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin DEFAULT NULL,",
		"DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
	} {
		if !strings.Contains(got, part) {
			t.Errorf("SHOW CREATE TABLE lacks %q:\n%s", part, got)
		}
	}
	// Other 0900 collations are unknown to mysqlite.
	_, err = conn.Execute("CREATE TABLE x (a VARCHAR(5) COLLATE utf8mb4_0900_as_cs)")
	requireMyError(t, err, 1273, "HY000")
}

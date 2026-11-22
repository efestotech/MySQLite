package wire

import (
	"regexp"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// COM_FIELD_LIST and COM_STATISTICS (docs/03-protocol.md section 4). `oracle: pending`.

func TestComFieldList(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, gadgets)

	fs, err := conn.FieldList("gadgets", "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range fs {
		names = append(names, string(f.Name))
	}
	if got := strings.Join(names, ","); got != "id,name,code,qty,price,note,added" {
		t.Errorf("all columns: %s", got)
	}
	if fs[0].Type != mysql.MYSQL_TYPE_LONG || fs[0].Flag&mysql.PRI_KEY_FLAG == 0 || string(fs[0].Table) != "gadgets" {
		t.Errorf("id field: %+v", fs[0])
	}
	if fs[2].Charset != 46 {
		t.Errorf("code charset: %d", fs[2].Charset)
	}

	fs, err = conn.FieldList("GADGETS", "N%")
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || string(fs[0].Name) != "name" || string(fs[1].Name) != "note" {
		t.Errorf("wildcard: %d fields", len(fs))
	}

	_, err = conn.FieldList("nope", "")
	requireMyError(t, err, 1146, "42S02")
	// The connection is still in sync.
	if n := execOK(t, conn, "SELECT id FROM items").RowNumber(); n != 2 {
		t.Errorf("after COM_FIELD_LIST: %d rows", n)
	}
}

func TestComStatistics(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "SELECT 1")

	conn.ResetSequence()
	if err := conn.WritePacket([]byte{0, 0, 0, 0, mysql.COM_STATISTICS}); err != nil {
		t.Fatal(err)
	}
	data, err := conn.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^Uptime: \d+  Threads: 1  Questions: \d+  Slow queries: 0  Opens: 0  Flush tables: 1  Open tables: 0  Queries per second avg: \d+\.\d{3}$`)
	if !re.Match(data) {
		t.Errorf("COM_STATISTICS: %q", data)
	}
	// go-mysql's OK packet was dropped: the next commands see their own responses.
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping after COM_STATISTICS: %v", err)
	}
	if n := execOK(t, conn, "SELECT id FROM items").RowNumber(); n != 2 {
		t.Errorf("after COM_STATISTICS: %d rows", n)
	}
}

package corpus

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// A column the case declares volatile (a creation time, a storage size) keeps its
// place but not its value, so two engines compare on everything else.
func TestTextRowsVolatile(t *testing.T) {
	row := append(mysql.PutLengthEncodedString([]byte("items")), mysql.PutLengthEncodedString([]byte("2026-11-24 19:35:30"))...)
	rs := &mysql.Resultset{
		Fields:   []*mysql.Field{{Name: []byte("Name")}, {Name: []byte("Create_time")}},
		RowDatas: []mysql.RowData{row},
	}
	got, err := textRows(rs, []string{"create_time"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `("items", "*")` {
		t.Errorf("volatile: %q", got)
	}
	if got, _ := textRows(rs, nil); got[0] != `("items", "2026-11-24 19:35:30")` {
		t.Errorf("no volatile: %q", got)
	}
}

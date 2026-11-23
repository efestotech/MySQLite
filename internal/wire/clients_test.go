package wire

import (
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Client scenarios of test/clients (docs/07-testing.md section 6) run against an
// in-process server when the client runtime is installed on the host; otherwise they
// are skipped and run in the docker client matrix (ADR 0002).

// TestClientPHPPDO is M4 criterion 1: PHP PDO with ATTR_EMULATE_PREPARES=false.
func TestClientPHPPDO(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php is not installed")
	}
	if out, err := exec.Command(php, "-r", `exit(extension_loaded("pdo_mysql") ? 0 : 1);`).CombinedOutput(); err != nil {
		t.Skipf("php has no pdo_mysql: %s", out)
	}
	addr := startServer(t, map[string]string{testUser: testPassword})
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(php, filepath.Join("..", "..", "test", "clients", "php-pdo", "scenario.php"))
	cmd.Env = append(cmd.Environ(), "MYSQLITE_HOST="+host, "MYSQLITE_PORT="+port,
		"MYSQLITE_USER="+testUser, "MYSQLITE_PASSWORD="+testPassword, "MYSQLITE_DB=shop")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "PASS") {
		t.Fatalf("scenario failed: %v", err)
	}
}

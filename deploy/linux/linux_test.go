// Package linux holds the files of the Linux packages: the systemd unit, the default
// configuration installed as /etc/mysqlite/mysqlite.yaml, and the Debian maintainer
// scripts that scripts/deb.sh puts in the .deb.
package linux

import (
	"os"
	"strings"
	"testing"

	"mysqlite/internal/config"
)

// The default configuration loads and serves on loopback from the directories the
// unit gives the service.
func TestDefaultConfig(t *testing.T) {
	cfg, err := config.Load([]string{"-config", "mysqlite.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1" || cfg.Port != 3307 || cfg.DataDir != "/var/lib/mysqlite" || cfg.Log.Dir != "/var/log/mysqlite" || len(cfg.Users) != 1 {
		t.Errorf("default config %+v", cfg)
	}
}

// The unit runs the binary the package installs, with the default configuration, as
// the mysqlite user, in the state and log directories the configuration names.
func TestServiceUnit(t *testing.T) {
	raw, err := os.ReadFile("mysqlite.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(raw)
	for _, want := range []string{
		"ExecStart=/usr/bin/mysqlite -config /etc/mysqlite/mysqlite.yaml",
		"User=mysqlite",
		"StateDirectory=mysqlite",
		"LogsDirectory=mysqlite",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("mysqlite.service: no %q", want)
		}
	}
}

// The maintainer scripts create the system user before the service starts, and the
// service is enabled and started only when systemd runs.
func TestMaintainerScripts(t *testing.T) {
	post, err := os.ReadFile("debian/postinst")
	if err != nil {
		t.Fatal(err)
	}
	p := string(post)
	user := strings.Index(p, "adduser --system")
	start := strings.Index(p, "systemctl enable --now")
	if user < 0 || start < 0 || user > start {
		t.Errorf("postinst must create the user (%d) before starting the service (%d)", user, start)
	}
	if !strings.Contains(p, "/run/systemd/system") {
		t.Error("postinst must not call systemctl without systemd")
	}
	for _, f := range []string{"debian/prerm", "debian/postrm"} {
		if _, err := os.Stat(f); err != nil {
			t.Error(err)
		}
	}
}

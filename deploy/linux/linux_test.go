// Package linux holds the files of the Linux packages: the systemd unit, the default
// configuration installed as /etc/mysqlite/mysqlite.yaml, the Debian maintainer
// scripts that scripts/deb.sh puts in the .deb, and the RPM spec scripts/rpm.sh builds.
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

// The RPM spec creates the system user before installing the files it owns, starts the
// service only on a first install with systemd running, keeps a changed configuration
// on upgrade, and installs the same unit and configuration as the .deb.
func TestRPMSpec(t *testing.T) {
	raw, err := os.ReadFile("rpm/mysqlite.spec")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	pre := strings.Index(spec, "%pre\n")
	user := strings.Index(spec, "useradd -r")
	post := strings.Index(spec, "%post\n")
	start := strings.Index(spec, "systemctl enable --now")
	if pre < 0 || user < pre || post < user || start < post {
		t.Errorf("the user must be created in %%pre (%d, %d) and the service started in %%post (%d, %d)", pre, user, post, start)
	}
	for _, want := range []string{
		"if [ \"$1\" -eq 1 ]; then",
		"[ -d /run/systemd/system ]",
		"%config(noreplace) %attr(0640,root,mysqlite) /etc/mysqlite/mysqlite.yaml",
		"/usr/lib/systemd/system/mysqlite.service",
		"%dir %attr(0750,mysqlite,mysqlite) /var/lib/mysqlite",
		"%dir %attr(0750,mysqlite,mysqlite) /var/log/mysqlite",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("mysqlite.spec: no %q", want)
		}
	}
}

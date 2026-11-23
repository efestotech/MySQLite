// Package docker holds the Docker topology (docs/07-testing.md section 2). Docker is not
// on the development host until the end (ADR 0002), so this test checks what it can
// without it: the compose file parses, has the services the docs name, and every file
// it mounts or runs exists in the repository.
package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type compose struct {
	Services map[string]struct {
		Build      any      `yaml:"build"`
		Image      string   `yaml:"image"`
		Volumes    []string `yaml:"volumes"`
		Entrypoint []string `yaml:"entrypoint"`
		Command    []string `yaml:"command"`
	} `yaml:"services"`
}

func TestCompose(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c compose
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mysqlite", "mysql-ref", "runner", "cli80", "cli84", "cli-maria", "php", "adminer", "java"} {
		if _, ok := c.Services[name]; !ok {
			t.Errorf("service %s is missing", name)
		}
	}
	for name, s := range c.Services {
		if s.Image == "" && s.Build == nil {
			t.Errorf("%s: neither image nor build", name)
		}
		// Bind mounts: host paths relative to this directory must exist.
		mounts := map[string]string{}
		for _, v := range s.Volumes {
			host, target, _ := strings.Cut(v, ":")
			target, _, _ = strings.Cut(target, ":")
			mounts[target] = host
			if !strings.HasPrefix(host, ".") || strings.Contains(host, "logs/") {
				continue // named volume, or a directory the run creates
			}
			if _, err := os.Stat(filepath.FromSlash(host)); err != nil {
				t.Errorf("%s: volume %s: %v", name, v, err)
			}
		}
		// Scripts run from a mounted directory must exist on the host side.
		for _, arg := range append(append([]string{}, s.Entrypoint...), s.Command...) {
			for target, host := range mounts {
				if rest, ok := strings.CutPrefix(arg, target+"/"); ok && strings.HasPrefix(host, ".") {
					if _, err := os.Stat(filepath.Join(filepath.FromSlash(host), filepath.FromSlash(rest))); err != nil {
						t.Errorf("%s: %s: %v", name, arg, err)
					}
				}
			}
		}
	}
	// Files the Dockerfile copies.
	for _, f := range []string{"../../go.mod", "../../go.sum", "../../LICENSE", "../../NOTICE", "mysqlite.yaml", "php.Dockerfile", "mysql-ref/init.sql"} {
		if _, err := os.Stat(filepath.FromSlash(f)); err != nil {
			t.Error(err)
		}
	}
}

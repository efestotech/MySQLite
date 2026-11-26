package docker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestReleaseWorkflow checks that .github/workflows/release.yml runs on version tags,
// tests first, builds a static package for each OS and architecture, and publishes
// them with checksums. It cannot run the workflow: that needs GitHub.
func TestReleaseWorkflow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On struct {
			Push struct {
				Tags []string `yaml:"tags"`
			} `yaml:"push"`
		} `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Needs    any `yaml:"needs"`
			Strategy struct {
				Matrix struct {
					Include []map[string]string `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wf.On.Push.Tags, []string{"v*"}) {
		t.Errorf("tags %v, want [v*]", wf.On.Push.Tags)
	}
	if wf.Permissions["contents"] != "write" {
		t.Errorf("permissions %v: a release needs contents: write", wf.Permissions)
	}

	var targets []string
	for _, inc := range wf.Jobs["package"].Strategy.Matrix.Include {
		targets = append(targets, inc["goos"]+"/"+inc["goarch"])
	}
	slices.Sort(targets)
	want := []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}
	if !slices.Equal(targets, want) {
		t.Errorf("package targets %v, want %v", targets, want)
	}
	if n := wf.Jobs["package"].Needs; n != "test" {
		t.Errorf("package needs %v, want test", n)
	}

	var all strings.Builder
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			all.WriteString(s.Uses + "\n" + s.Run + "\n")
		}
	}
	text := all.String()
	for _, want := range []string{
		"go vet ./...",
		"go test",
		`CGO_ENABLED=0`,
		"-trimpath",
		"statically linked",
		"README.md LICENSE NOTICE",
		"deploy/mysqlite.example.yaml",
		"tar -czf",
		"zip -r",
		"SHA256SUMS",
		"gh release create",
		"scripts/deb.sh",
		"*.deb",
		"deploy/linux/mysqlite.service",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("release.yml: no step with %q", want)
		}
	}
}

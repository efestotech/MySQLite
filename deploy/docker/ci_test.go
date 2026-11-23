package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestCIWorkflow checks that .github/workflows/ci.yml parses and runs every step
// docs/07-testing.md section 8 asks for. It cannot run the workflow: that needs GitHub.
func TestCIWorkflow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix map[string][]string `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			all.WriteString(s.Uses + "\n" + s.Run + "\n")
			for _, v := range s.With {
				all.WriteString(v + "\n")
			}
		}
	}
	text := all.String()
	for _, want := range []string{
		"go vet ./...",
		"golangci/golangci-lint-action",
		"go test -race",
		"go-licenses\" check",
		"go run ./scripts/notice -check",
		"CGO_ENABLED",
		"statically linked",
		"scripts/dev.sh harness",
		"scripts/dev.sh clients",
		"logs/harness/report.json",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ci.yml: no step with %q", want)
		}
	}
	if got := strings.Join(wf.Jobs["build"].Strategy.Matrix["goarch"], ","); got != "amd64,arm64" {
		t.Errorf("build matrix goarch = %q, want amd64,arm64", got)
	}
	// Every script the workflow runs exists.
	for _, p := range []string{"scripts/dev.sh", "scripts/notice/main.go", "cmd/mysqlite/main.go"} {
		if _, err := os.Stat(filepath.Join("..", "..", p)); err != nil {
			t.Error(err)
		}
	}
}

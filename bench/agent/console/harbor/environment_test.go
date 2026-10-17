package harbor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTaskEnvironment(t *testing.T) {
	root := t.TempDir()
	tasks := filepath.Join(root, "harbor", "tasks")
	task := filepath.Join(tasks, "demo-kavach")
	writeFile(t, filepath.Join(task, "task.toml"), `
[agent]
network_mode = "no-network"

[environment]
docker_image = "ghcr.io/kavachlabs/bench-demo:kavach-0123456789ab"
build_timeout_sec = 1800.0
workdir = "/work"

[[environment.mcp_servers]]
name = "kavach"

[verifier]
environment_mode = "separate"

[verifier.environment]
docker_image = "ghcr.io/kavachlabs/bench-demo:grader-0123456789ab"
`)
	compose := "# comment\nservices:\n  main:\n    volumes:\n      - ./:/tests\n"
	writeFile(t, filepath.Join(task, "environment", "docker-compose.yaml"), compose)
	writeFile(t, filepath.Join(task, "tests", "docker-compose.yaml"), compose)
	writeFile(t, filepath.Join(root, "tasks", "demo", "Dockerfile"), "FROM scratch AS kavach\nFROM kavach AS grader\n")

	env, err := TaskEnvironment(tasks, "demo-kavach")
	if err != nil {
		t.Fatal(err)
	}
	if env.Network != "no-network" || env.Workdir != "/work" || env.VerifierMode != "separate" || env.BuildTimeoutSec != 1800 {
		t.Errorf("settings = %+v", env)
	}
	if len(env.MCPServers) != 1 || env.MCPServers[0] != "kavach" {
		t.Errorf("mcp servers = %v", env.MCPServers)
	}
	var roles []string
	for _, f := range env.Files {
		roles = append(roles, f.Role)
		if f.Missing {
			t.Errorf("%s (%s) missing", f.Role, f.Path)
		}
	}
	// Both images are targets of the same Dockerfile, which is listed once.
	if got := strings.Join(roles, ","); got != "agent,verifier,dockerfile,task" {
		t.Fatalf("roles = %s", got)
	}
	dockerfile := filepath.Join(root, "tasks", "demo", "Dockerfile")
	if f := env.Files[0]; f.Target != "kavach" || f.Dockerfile != dockerfile || f.Image != "ghcr.io/kavachlabs/bench-demo:kavach-0123456789ab" {
		t.Errorf("agent compose = image %q, target %q, dockerfile %q; want kavach, %s", f.Image, f.Target, f.Dockerfile, dockerfile)
	}
	if f := env.Files[1]; f.Target != "grader" {
		t.Errorf("verifier target = %q", f.Target)
	}
	if !strings.Contains(env.Files[2].Content, "AS grader") {
		t.Errorf("dockerfile content = %q", env.Files[2].Content)
	}

	for _, name := range []string{"", "..", "../tasks", "nope", ".hidden"} {
		if _, err := TaskEnvironment(tasks, name); !errors.Is(err, ErrNoTask) {
			t.Errorf("TaskEnvironment(%q) error = %v, want ErrNoTask", name, err)
		}
	}
}

func TestSplitImage(t *testing.T) {
	for ref, want := range map[string][3]string{
		"ghcr.io/kavachlabs/bench-redis-10068:kavach-implement-0123456789ab": {"redis-10068", "kavach-implement", "true"},
		"ghcr.io/kavachlabs/bench-axum-1934:grader-0123456789ab":             {"axum-1934", "grader", "true"},
		"kavach-bench/redis-10068:kavach":                                    {"", "", "false"},
		"":                                                                   {"", "", "false"},
	} {
		task, target, ok := splitImage(ref)
		if got := [3]string{task, target, fmt.Sprint(ok)}; got != want {
			t.Errorf("splitImage(%q) = %v, want %v", ref, got, want)
		}
	}
}

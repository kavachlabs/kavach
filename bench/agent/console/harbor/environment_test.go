package harbor

import (
	"errors"
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
build_timeout_sec = 1800.0
workdir = "/work"

[[environment.mcp_servers]]
name = "kavach"

[verifier]
environment_mode = "separate"
`)
	compose := func(target string) string {
		return `# comment
services:
  main:
    build:
      context: ${KAVACH_TEST_ROOT_UNSET:-../../../..}
      dockerfile: tasks/demo/Dockerfile
      target: ` + target + `
    volumes:
      - ./:/tests
`
	}
	writeFile(t, filepath.Join(task, "environment", "docker-compose.yaml"), compose("kavach"))
	writeFile(t, filepath.Join(task, "tests", "docker-compose.yaml"), compose("grader"))
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
	// Both compose files build the same Dockerfile, which is listed once.
	if got := strings.Join(roles, ","); got != "agent,verifier,dockerfile,task" {
		t.Fatalf("roles = %s", got)
	}
	dockerfile := filepath.Join(root, "tasks", "demo", "Dockerfile")
	if f := env.Files[0]; f.Target != "kavach" || f.Dockerfile != dockerfile {
		t.Errorf("agent compose = target %q, dockerfile %q; want kavach, %s", f.Target, f.Dockerfile, dockerfile)
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

func TestExpandEnv(t *testing.T) {
	t.Setenv("KAVACH_TEST_SET", "/repo")
	t.Setenv("KAVACH_TEST_EMPTY", "")
	for in, want := range map[string]string{
		"${KAVACH_TEST_SET:-../..}":   "/repo",
		"${KAVACH_TEST_EMPTY:-../..}": "../..",
		"${KAVACH_TEST_EMPTY}":        "",
		"$KAVACH_TEST_SET/x":          "/repo/x",
		"plain/path":                  "plain/path",
	} {
		if got := expandEnv(in); got != want {
			t.Errorf("expandEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

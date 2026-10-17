package harbor

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactWriterSplitSecret(t *testing.T) {
	secret := "sk-or-v1-0123456789abcdef"
	input := "start " + secret + " middle \"" + secret + "\" end " + secret
	for cut := 0; cut <= len(input); cut++ {
		var out bytes.Buffer
		w := newRedactWriter(&out, []string{secret, "short"})
		w.Write([]byte(input[:cut]))
		w.Write([]byte(input[cut:]))
		w.Close()
		if got := out.String(); strings.Contains(got, "0123456789") || strings.Count(got, "[redacted]") != 3 || !strings.HasPrefix(got, "start [redacted] middle") {
			t.Fatalf("cut %d: %q", cut, got)
		}
	}
	var out bytes.Buffer
	w := newRedactWriter(&out, []string{"a\"b-secret-value"})
	w.Write([]byte(`{"k":"a\"b-secret-value"} a"b-secret-value`))
	w.Close()
	if strings.Contains(out.String(), "secret-value") {
		t.Fatalf("escaped form leaked: %q", out.String())
	}
}

func TestIsolatedRunLogAndHome(t *testing.T) {
	dir := t.TempDir()
	harborCmd := filepath.Join(dir, "harbor")
	os.WriteFile(harborCmd, []byte("#!/bin/sh\necho \"key=$MY_KEY home=$HOME\"\nprintf 'half-'; sleep 0.1; echo \"$MY_KEY\"\nexit 3\n"), 0o755)
	tasks := filepath.Join(dir, "tasks")
	os.MkdirAll(filepath.Join(tasks, "t"), 0o755)
	os.WriteFile(filepath.Join(tasks, "t", "task.toml"), nil, 0o644)
	t.Setenv("OPERATOR_SECRET", "must-not-reach-harbor")
	r := &Runner{Harbor: harborCmd, JobsDir: filepath.Join(dir, "jobs"), TasksDir: tasks}
	id, err := r.Start(RunRequest{Tasks: []string{"t"}, Env: map[string]string{"MY_KEY": "sk-visitor-0123456789abcdef"}})
	if err != nil {
		t.Fatal(err)
	}
	r.Wait(id)
	log, _ := os.ReadFile(r.file(id, ".log"))
	if strings.Contains(string(log), "0123456789abcdef") || strings.Count(string(log), "[redacted]") != 2 || !strings.Contains(string(log), "exit status 3") {
		t.Fatalf("log %q", log)
	}
	home := r.file("home/"+id, "")
	if !strings.Contains(string(log), "home="+home) {
		t.Errorf("harbor did not get its own HOME %s: %q", home, log)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("home survives the run: %v", err)
	}
}

// TestIsolatedHarborStarts checks the installed Harbor, if there is one,
// still starts in the environment a visitor's run gets. It calls no provider.
func TestIsolatedHarborStarts(t *testing.T) {
	path, err := exec.LookPath("harbor")
	if err != nil {
		t.Skip("no harbor installed")
	}
	home := t.TempDir()
	cmd := exec.Command(path, "--version")
	cmd.Env = isolatedEnv(home, map[string]string{"OPENROUTER_API_KEY": "x"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("harbor --version with an isolated HOME: %v\n%s", err, out)
	}
	cmd = exec.Command(path, "run", "--help")
	cmd.Env = isolatedEnv(home, nil)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("harbor run --help with an isolated HOME: %v\n%s", err, out)
	}
}

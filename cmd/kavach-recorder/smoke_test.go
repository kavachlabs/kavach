package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/internal/envfacts"
	"github.com/kavachlabs/kavach/internal/recstream"
	"github.com/kavachlabs/kavach/journal"
)

// TestSmoke runs the recorder as an SDK would, on the real host: a genesis
// journal, a few steps with a panic, signals that must be ignored, and a
// fixture that the kavach CLI can read.
func TestSmoke(t *testing.T) {
	bin := buildRecorder(t)
	dir := t.TempDir()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "SMOKE_VAR=hello", "SMOKE_API_TOKEN=s3cret")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	msgs := make(chan map[string]any, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				msgs <- m
			}
		}
		close(msgs)
	}()
	wait := func(typ string) map[string]any {
		t.Helper()
		for {
			select {
			case m, ok := <-msgs:
				if !ok {
					t.Fatalf("control stream ended before %q", typ)
				}
				if m["t"] == typ {
					return m
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("no %q message", typ)
			}
		}
	}

	enc := recstream.NewEncoder(stdin)
	if err := enc.Open(recstream.Open{Service: "smoke", Start: journal.StartGenesis, Dir: dir, Producer: "smoke-test"}); err != nil {
		t.Fatal(err)
	}
	ready := wait("ready")
	enc.Facts([]journal.Fact{{Key: "host.runtime", Form: journal.FactValue, Value: []byte("smoke-1.0")}})
	enc.Records([]journal.Record{{Type: journal.TypeInput, Source: "test", Position: "0", Data: []byte("a")}, {Type: journal.TypeClock, UnixNanos: 1}}, true)

	// A signal to the service's process group must not stop the recorder.
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		cmd.Process.Signal(sig)
	}
	time.Sleep(50 * time.Millisecond)

	enc.Records([]journal.Record{
		{Type: journal.TypeInput, Source: "test", Position: "1", Data: []byte("bad")},
		{Type: journal.TypeMarker, Kind: journal.MarkerPanic, Message: "boom", Data: []byte("stack")},
	}, true)
	fx := wait("fixture")
	if fx["seq"] != "3" || fx["failure"] != "panic: boom" {
		t.Fatalf("fixture message = %v", fx)
	}
	enc.Close()
	wait("closed")
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	file := fx["file"].(string)
	if filepath.Dir(file) != filepath.Join(dir, "fixtures") {
		t.Fatalf("fixture in %s", file)
	}
	if !strings.HasPrefix(ready["file"].(string), dir) || ready["recorder"] == "" {
		t.Fatalf("ready = %v", ready)
	}
	j, err := journal.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]journal.Fact{}
	for _, f := range j.Records[0].Facts {
		env[f.Key] = f
	}
	if string(env["env.SMOKE_VAR"].Value) != "hello" || env["env.SMOKE_API_TOKEN"].Form != journal.FactSHA256 {
		t.Errorf("env facts: %+v %+v", env["env.SMOKE_VAR"], env["env.SMOKE_API_TOKEN"])
	}
	if string(env["host.runtime"].Value) != "smoke-1.0" || string(env["host.os"].Value) == "" {
		t.Errorf("host facts: %+v", env)
	}

	out, err := exec.Command("go", "run", "../kavach", "inspect", file).CombinedOutput()
	if err != nil {
		t.Fatalf("kavach inspect: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "panic  boom") || !strings.Contains(string(out), "environment") {
		t.Fatalf("kavach inspect:\n%s", out)
	}
}

func runFactsCmd(t *testing.T, bin string, env []string, args ...string) map[string]envfacts.JSONFact {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"facts"}, args...)...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	var m map[string]envfacts.JSONFact
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("facts output: %v\n%s", err, out)
	}
	return m
}

func TestFactsCommand(t *testing.T) {
	bin := buildRecorder(t)
	m := runFactsCmd(t, bin, []string{"PLAIN=y", "MY_SECRET=z", "CUSTOM=c"}, "--secret-key", "CUSTOM")
	if f := m["env.PLAIN"]; f.Value == nil || string(*f.Value) != "y" {
		t.Errorf("PLAIN = %+v", f)
	}
	for _, k := range []string{"env.MY_SECRET", "env.CUSTOM"} {
		if m[k].SHA256 == nil || m[k].Value != nil {
			t.Errorf("%s = %+v", k, m[k])
		}
	}
	if m["host.os"].Value == nil {
		t.Error("no host.os")
	}
	if _, ok := m["host.runtime"]; ok {
		t.Error("facts printed host.runtime")
	}

	tf := filepath.Join(t.TempDir(), "facts.json")
	os.WriteFile(tf, []byte(`{"run":"r","facts":{"env.A":{"value":"MQ=="},"host.gone":{"unset":true}}}`), 0o644)
	m = runFactsCmd(t, bin, []string{"PLAIN=y"}, "--test-facts", tf)
	if len(m) != 2 || string(*m["env.A"].Value) != "1" || !m["host.gone"].Unset {
		t.Errorf("test facts = %+v", m)
	}
}

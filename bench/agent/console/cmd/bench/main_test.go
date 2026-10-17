package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{"nope"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "bench api") {
			t.Errorf("%v: exit %d\n%s", args, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"api", "-port", "x"}, &out, &errb); code != 2 {
		t.Errorf("bad flag: exit %d", code)
	}
	errb.Reset()
	if code := run([]string{"api", "-addr", "0.0.0.0:0"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "-trial") {
		t.Errorf("public address without -trial: exit %d\n%s", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"api", "-trial", "-trust-cf", "-addr", "0.0.0.0:0"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "-trust-cf") {
		t.Errorf("-trust-cf on a public address: exit %d\n%s", code, errb.String())
	}
}

func TestJobsWarning(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Colima's home-folder sharing is a Mac concern")
	}
	home, _ := os.UserHomeDir()
	for dir, warn := range map[string]bool{
		home:                                   false,
		filepath.Join(home, "jobs"):            false,
		filepath.Join(home, "..", "jobs"):      true,
		"/private/tmp/jobs":                    true,
		filepath.Join(filepath.Dir(home), "x"): true,
	} {
		if got := jobsWarning(dir) != ""; got != warn {
			t.Errorf("jobsWarning(%s) warns: %v, want %v", dir, got, warn)
		}
	}
}

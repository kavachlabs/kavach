package api

import (
	"slices"
	"testing"
)

func TestScrubPathsFileURLs(t *testing.T) {
	rules := []pathRule{{[]byte("/Users/op"), []byte("~")}, {[]byte("/srv/jobs"), []byte("<jobs>")}}
	for in, want := range map[string]string{
		"file:///Users/op/.cache/x":   "file://~/.cache/x",
		`"file:///srv/jobs/a/b.json"`: `"file://<jobs>/a/b.json"`,
		"http://host/Users/op":        "http://host/Users/op",
		"//Users/op":                  "//Users/op",
	} {
		if got := string(scrubPaths(rules, []byte(in))); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestServerSecretsCoverFileAndEnvironment(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-from-the-environment-1")
	e := newTrialServer(t, "", nil) // its env file holds another OPENROUTER_API_KEY
	got := e.s.trial.serverSecrets
	for _, want := range []string{"sk-or-v1-from-the-environment-1", "sk-or-v1-server-secret-9999", "r2-server-secret-7777"} {
		if !slices.Contains(got, want) {
			t.Errorf("serverSecrets lacks %q: %q", want, got)
		}
	}
}

func TestTrialPrunesFinishedRunSecrets(t *testing.T) {
	e := newTrialServer(t, "", nil)
	a := e.visitor()
	e.setKeys(a)
	first := e.start(a)
	e.s.trial.mu.Lock()
	n := len(e.s.trial.jobSecrets[first])
	e.s.trial.mu.Unlock()
	if n == 0 {
		t.Fatal("no secrets kept for a live run")
	}
	e.do(a, "POST", "/api/jobs/"+first+"/stop", "")
	e.s.runner.Wait(first)
	second := e.start(a)
	e.s.trial.mu.Lock()
	defer e.s.trial.mu.Unlock()
	if _, ok := e.s.trial.jobSecrets[first]; ok {
		t.Error("secrets of a finished run kept")
	}
	if len(e.s.trial.jobSecrets[second]) == 0 {
		t.Error("secrets of the live run dropped")
	}
}

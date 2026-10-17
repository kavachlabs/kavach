package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScrubPathsBoundaries(t *testing.T) {
	rules := []pathRule{}
	for _, r := range [][2]string{{"/srv/bench/jobs", "<jobs>"}, {"/root", "~"}, {"/app", "<root>"}} {
		rules = append(rules, pathRule{[]byte(r[0]), []byte(r[1])})
	}
	for in, want := range map[string]string{
		"/srv/bench/jobs/x/log.txt":    "<jobs>/x/log.txt",
		"in /srv/bench/jobs.":          "in <jobs>.",
		`"/srv/bench/jobs"`:            `"<jobs>"`,
		`line\n/root/.cache/uv`:        `line\n~/.cache/uv`,
		"/rootfs/etc /root /root/a b":  "/rootfs/etc ~ ~/a b",
		"/application/x /usr/app/y":    "/application/x /usr/app/y",
		"/app/src/main.c":              "<root>/src/main.c",
		"/root2 /root-x /root.bak":     "/root2 /root-x /root.bak",
		"/srv/bench/jobsX /srv/bench/": "/srv/bench/jobsX /srv/bench/",
		"nothing here":                 "nothing here",
	} {
		if got := string(scrubPaths(rules, []byte(in))); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestTrialScrubsSecretsInServedFiles(t *testing.T) {
	e := newTrialServer(t, "", nil)
	e.s.trial.mu.Lock()
	e.s.trial.showcase["redis-oracle"] = true
	e.s.trial.mu.Unlock()
	patchPath := "/api/trials/redis-oracle/redis-10068-kavach__Dw5kmkd/patch"
	_, raw, _ := e.raw(e.visitor(), "GET", patchPath, "")
	secret, _, _ := strings.Cut(string(raw), "\n")
	if len(secret) < minKeyLen {
		t.Fatalf("fixture patch starts %q", secret)
	}
	// A run's own secrets are hidden in that run's files only.
	e.s.trial.mu.Lock()
	e.s.trial.jobSecrets["redis-oracle"] = []string{secret}
	e.s.trial.mu.Unlock()
	_, raw, _ = e.raw(e.visitor(), "GET", patchPath, "")
	if strings.Contains(string(raw), secret) || !strings.HasPrefix(string(raw), "[redacted]") {
		t.Errorf("patch %.100s", raw)
	}
	_, raw, _ = e.raw(e.visitor(), "GET", "/api/trials/redis-oracle/redis-10068-kavach__Dw5kmkd/steps", "")
	if !json.Valid(raw) {
		t.Errorf("steps are not JSON: %.200s", raw)
	}

	// The server's own are hidden everywhere.
	e.s.trial.mu.Lock()
	delete(e.s.trial.jobSecrets, "redis-oracle")
	e.s.trial.serverSecrets = append(e.s.trial.serverSecrets, secret)
	e.s.trial.mu.Unlock()
	_, raw, _ = e.raw(e.visitor(), "GET", patchPath, "")
	if strings.Contains(string(raw), secret) {
		t.Errorf("server secret served: %.100s", raw)
	}
}

func TestTrialKeepsOwnershipWhenKeysAreEnteredAgain(t *testing.T) {
	e := newTrialServer(t, "", nil)
	c := e.visitor()
	e.setKeys(c)
	id := e.start(c)

	e.setKeys(c)
	var job map[string]any
	_, raw, h := e.raw(c, "GET", "/api/jobs/"+id, "")
	json.Unmarshal(raw, &job)
	if job["stoppable"] != true {
		t.Errorf("owner lost the run: %v", job)
	}
	_, raw, _ = e.raw(c, "GET", "/api/trial/session", "")
	var v sessionView
	json.Unmarshal(raw, &v)
	if len(v.Runs) != 1 || v.Runs[0] != id || v.HasKeys != true {
		t.Errorf("session %s", raw)
	}
	cookie := h.Get("Set-Cookie")
	sid, _, _ := strings.Cut(strings.TrimPrefix(cookie, "kv_session="), ";")
	sum := sha256.Sum256([]byte(sid))
	owner, _ := os.ReadFile(filepath.Join(e.jobs, ".bench", id+".owner"))
	if string(owner) != hex.EncodeToString(sum[:]) {
		t.Errorf("owner file %q does not follow the new session", owner)
	}
	// Still one live run per session.
	if code, _ := e.do(c, "POST", "/api/jobs", runBody); code != 429 {
		t.Errorf("second run: %d", code)
	}
	if code, _ := e.do(c, "POST", "/api/jobs/"+id+"/stop", ""); code != 202 {
		t.Errorf("stop: %d", code)
	}
	// Another visitor's session does not get it.
	d := e.visitor()
	e.setKeys(d)
	if code, _ := e.do(d, "POST", "/api/jobs/"+id+"/stop", ""); code != 403 {
		t.Errorf("stop by other: %d", code)
	}
}

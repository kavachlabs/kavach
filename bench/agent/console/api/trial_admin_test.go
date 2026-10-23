package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (e *trialEnv) admin(body string) (int, []AdminRetried, string) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.s.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/retry", strings.NewReader(body)))
	var out []AdminRetried
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func (e *trialEnv) attempts(id string, names ...string) {
	dir := filepath.Join(e.jobs, id)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"job_name":"`+id+`"}`), 0o644)
	for _, n := range names {
		os.MkdirAll(filepath.Join(dir, n), 0o755)
		os.WriteFile(filepath.Join(dir, n, "config.json"), []byte("{}"), 0o644)
		os.WriteFile(filepath.Join(dir, n, "result.json"), []byte("{}"), 0o644)
	}
}

func TestTrialAdminRetry(t *testing.T) {
	e, own := ownServer(t, callsScript)
	c := e.visitor()
	e.invite(c)
	id := e.start(c)
	e.call(1)
	e.stop(c, id)
	e.attempts(id, "hello-world-kavach__aB3", "x__dup", "y__dup")

	// The public listener has no operator routes.
	if code, _, _ := e.raw(c, "POST", "/retry", `{"job":"`+id+`","trials":["aB3"]}`, "Content-Type", "application/json"); code == http.StatusOK {
		t.Fatalf("the public server serves /retry")
	}
	for _, body := range []string{``, `{"job":"` + id + `"}`, `{"job":"../x","trials":["a"]}`} {
		if code, _, raw := e.admin(body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, code, raw)
		}
	}
	code, out, raw := e.admin(`{"job":"` + id + `","trials":["aB3","nope","dup","../etc"]}`)
	if code != 200 || len(out) != 4 {
		t.Fatalf("retry: %d %s", code, raw)
	}
	if out[0].Trial != "hello-world-kavach__aB3" || out[0].Error != "" || out[0].Queued {
		t.Errorf("by suffix: %+v", out[0])
	}
	for _, r := range out[1:] {
		if r.Error == "" {
			t.Errorf("%s retried: %+v", r.Trial, r)
		}
	}
	if !strings.Contains(out[2].Error, "more than one") {
		t.Errorf("ambiguous suffix: %+v", out[2])
	}
	if env := e.call(2); !strings.Contains(env, "sk-or-v1-server-secret-9999") || !strings.Contains(env, "jobs resume") {
		t.Errorf("retry env:\n%s", env)
	}
	// Asked again while that retry runs, it waits for it.
	code, out, raw = e.admin(`{"job":"` + id + `","trials":["dup","x__dup"]}`)
	if code != 200 || out[1].Error != "" || !out[1].Queued {
		t.Errorf("queued: %d %s", code, raw)
	}
	e.stop(c, id)

	// A run on a visitor's own keys can't be retried: the server has none.
	oid := e.startOwn(own)
	e.call(3)
	e.stop(own, oid)
	e.attempts(oid, oid+"__A")
	if code, _, raw := e.admin(`{"job":"` + oid + `","trials":["A"]}`); code != http.StatusConflict {
		t.Errorf("own-key run: %d %s", code, raw)
	}
}

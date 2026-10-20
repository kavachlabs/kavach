package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// The trial's sessions as a Kavach service: each input is a visitor's request
// to a trial server, or the server restarting, and each output is the response
// as that visitor sees it. The server is the real one (New, in trial mode),
// over a directory of its own, with a fake Harbor that leaves a finished run
// behind; cmd/sessions hosts it. Fixtures and contracts of the session code
// then replay like any service's.

// SessionInput is one input of SessionService.
type SessionInput struct {
	// Restart starts a new server over the same jobs directory and settings,
	// as a deploy does, and drops the old one.
	Restart bool `json:"restart,omitempty"`
	// Wait waits for every run started so far to end.
	Wait bool `json:"wait,omitempty"`
	// Visitor names whose cookies the request carries; Cookie, when set,
	// replaces them with one the server never issued.
	Visitor string          `json:"visitor,omitempty"`
	Cookie  string          `json:"cookie,omitempty"`
	IP      string          `json:"ip,omitempty"`
	Method  string          `json:"method,omitempty"`
	Path    string          `json:"path,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// SessionService is the Kavach handler.
type SessionService struct {
	// Tasks is the tasks directory (harbor/testdata/tasks, which has the
	// hello-world tasks).
	Tasks string

	root    string // the server's jobs, settings and Harbor
	cfg     Config
	srv     *Server
	models  *httptest.Server
	jars    map[string]map[string]string // visitor -> cookie name -> value
	runs    []string                     // run ids in the order they started; outputs name them run-1, ...
	starter map[string]string            // run id -> the visitor who started it
	keys    []string                     // every key value a visitor sent
	// keyed holds the visitors who have entered keys since the last restart.
	keyed    map[string]bool
	restarts int
	seen     []seenResponse
}

type seenResponse struct {
	step     int
	visitor  string
	path     string
	view     *sessionView
	job      *jobFlags
	restarts int
	// keyed is whether the visitor had entered keys since the last restart
	// when the server answered.
	keyed bool
}

type jobFlags struct {
	ID        string `json:"id"`
	Stoppable bool   `json:"stoppable"`
	Resumable bool   `json:"resumable"`
	Retryable bool   `json:"retryable"`
}

// fakeHarbor leaves a run that ended behind: the job's config.json and one
// attempt folder, which is what retrying an attempt needs.
const fakeHarbor = `#!/bin/sh
[ "$1" = --version ] && { echo 'harbor 0.24.0'; exit 0; }
[ "$1" = run ] || exit 0
exec python3 - "$3" <<'PY'
import json, os, sys
c = json.load(open(sys.argv[1]))
job = os.path.join(c["jobs_dir"], c["job_name"])
attempt = os.path.join(job, c["job_name"] + "__A")
os.makedirs(attempt, exist_ok=True)
json.dump({"job_name": c["job_name"]}, open(os.path.join(job, "config.json"), "w"))
json.dump({}, open(os.path.join(attempt, "config.json"), "w"))
PY
`

func (h *SessionService) setup() error {
	// A host replaying for the kavach CLI exits without Close; its directory
	// goes on a later start.
	old, _ := filepath.Glob(filepath.Join(os.TempDir(), "kavach-sessions-*"))
	for _, p := range old {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Hour {
			os.RemoveAll(p)
		}
	}
	root, err := os.MkdirTemp("", "kavach-sessions-")
	if err != nil {
		return err
	}
	h.root = root
	jobs := filepath.Join(root, "jobs")
	if err := os.MkdirAll(filepath.Join(jobs, ".bench"), 0o755); err != nil {
		return err
	}
	harborCmd := filepath.Join(root, "harbor")
	if err := os.WriteFile(harborCmd, []byte(fakeHarbor), 0o755); err != nil {
		return err
	}
	envFile := filepath.Join(root, "server.env")
	if err := os.WriteFile(envFile, []byte("OPENROUTER_API_KEY=sk-or-v1-server-secret-9999\nDAYTONA_API_KEY=dtn-server-secret-8888\n"), 0o600); err != nil {
		return err
	}
	h.models = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"z-ai/glm-5.3"}]}`)
	}))
	tasks, err := filepath.Abs(cmpOr(h.Tasks, "harbor/testdata/tasks"))
	if err != nil {
		return err
	}
	h.cfg = Config{Harbor: harborCmd, JobsDir: jobs, TasksDir: tasks, EnvFile: envFile, ModelsURL: h.models.URL}
	h.jars, h.starter, h.keyed = map[string]map[string]string{}, map[string]string{}, map[string]bool{}
	h.start()
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// start serves a new server over the same directories, as a restart does.
func (h *SessionService) start() {
	cfg := h.cfg
	cfg.Trial = &TrialConfig{
		Models: []string{"openrouter/z-ai/glm-5.3"}, Tasks: []string{"hello-world-kavach"},
		InviteCodes: []string{"alpha-code"}, MaxAttempts: 2, MaxRunning: 3, InviteRuns: 5, RatePerMin: 1000,
	}
	h.srv = New(cfg)
}

// Close removes the server's directory.
func (h *SessionService) Close() {
	if h.models != nil {
		h.models.Close()
	}
	if h.root != "" {
		os.RemoveAll(h.root)
	}
}

func (h *SessionService) Handle(env kavach.Env, in kavach.Input) error {
	var req SessionInput
	if err := json.Unmarshal(in.Data, &req); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	if h.srv == nil {
		if err := h.setup(); err != nil {
			return err
		}
	}
	switch {
	case req.Restart:
		h.restarts++
		h.keyed = map[string]bool{}
		h.start()
		env.Emit("server", []byte(`{"restarted":true}`))
		return nil
	case req.Wait:
		for _, id := range h.runs {
			for end := time.Now().Add(10 * time.Second); h.srv.runner.Alive(id); time.Sleep(20 * time.Millisecond) {
				if time.Now().After(end) {
					return fmt.Errorf("run %s did not end", h.label(id))
				}
			}
		}
		env.Emit("server", []byte(`{"waited":true}`))
		return nil
	}
	return h.request(env, req)
}

func (h *SessionService) request(env kavach.Env, req SessionInput) error {
	path := h.unlabel(req.Path)
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader([]byte(h.unlabel(string(req.Body))))
		var fields map[string]any
		if json.Unmarshal(req.Body, &fields) == nil {
			for name, v := range fields {
				if s, ok := v.(string); ok && (strings.HasSuffix(name, "_key") || strings.HasPrefix(name, "modal_token")) {
					h.keys = append(h.keys, s)
				}
			}
		}
	}
	r := httptest.NewRequest(req.Method, path, body)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = net.JoinHostPort(cmpOr(req.IP, "203.0.113.10"), "40000")
	if req.Cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: req.Cookie})
	} else {
		names := make([]string, 0, len(h.jars[req.Visitor]))
		for name := range h.jars[req.Visitor] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			r.AddCookie(&http.Cookie{Name: name, Value: h.jars[req.Visitor][name]})
		}
	}
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	res := w.Result()
	if req.Cookie == "" && req.Visitor != "" {
		for _, c := range res.Cookies() {
			if h.jars[req.Visitor] == nil {
				h.jars[req.Visitor] = map[string]string{}
			}
			if c.MaxAge < 0 || c.Value == "" {
				delete(h.jars[req.Visitor], c.Name)
			} else {
				h.jars[req.Visitor][c.Name] = c.Value
			}
		}
	}
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{"visitor": req.Visitor, "status": res.StatusCode}
	seen := seenResponse{step: len(h.seen), visitor: req.Visitor, path: req.Path, restarts: h.restarts}
	switch {
	case req.Method == "POST" && path == "/api/jobs" && res.StatusCode == http.StatusAccepted:
		var v struct {
			ID string `json:"id"`
		}
		json.Unmarshal(raw, &v)
		h.runs = append(h.runs, v.ID)
		h.starter[v.ID] = req.Visitor
		out["id"] = h.label(v.ID)
	case strings.HasPrefix(path, "/api/trial/") && res.StatusCode == http.StatusOK:
		var v sessionView
		json.Unmarshal(raw, &v)
		if path == "/api/trial/keys" && req.Method == "POST" {
			h.keyed[req.Visitor] = true
		}
		runs := make([]string, len(v.Runs))
		for i, id := range v.Runs {
			runs[i] = h.label(id)
		}
		out["session"] = map[string]any{"has_keys": v.HasKeys, "provider": v.Provider, "invite": v.Invite, "runs": runs, "invite_runs_left": v.InviteRunsLeft}
		seen.view, seen.keyed = &v, h.keyed[req.Visitor]
	case req.Method == "GET" && strings.HasPrefix(path, "/api/jobs/") && res.StatusCode == http.StatusOK:
		var f jobFlags
		json.Unmarshal(raw, &f)
		out["job"] = map[string]any{"id": h.label(f.ID), "stoppable": f.Stoppable, "resumable": f.Resumable, "retryable": f.Retryable}
		seen.job = &f
	default:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			out["error"] = h.labelAll(e.Error)
		}
	}
	h.seen = append(h.seen, seen)
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	env.Emit("http", b)
	return nil
}

// label names a run by the order it started in, since its id is random.
func (h *SessionService) label(id string) string {
	if i := slices.Index(h.runs, id); i >= 0 {
		return fmt.Sprintf("run-%d", i+1)
	}
	if id == "" {
		return ""
	}
	return "run-?"
}

var runLabel = regexp.MustCompile(`run-(\d+)`)

// unlabel puts the run ids back for the labels in an input.
func (h *SessionService) unlabel(s string) string {
	return runLabel.ReplaceAllStringFunc(s, func(m string) string {
		var n int
		fmt.Sscanf(m, "run-%d", &n)
		if n >= 1 && n <= len(h.runs) {
			return h.runs[n-1]
		}
		return m
	})
}

func (h *SessionService) labelAll(s string) string {
	for i, id := range h.runs {
		s = strings.ReplaceAll(s, id, fmt.Sprintf("run-%d", i+1))
	}
	return s
}

// Invariants are the specification's (SPEC.md under
// testdata/kavach/sessions), stated on what the server answered and wrote.
func (h *SessionService) Invariants() []kavach.Invariant {
	return []kavach.Invariant{
		{Name: "keys-never-on-disk", Check: func() error {
			// No file the server wrote holds a key a visitor sent.
			if h.root == "" {
				return nil
			}
			return filepath.WalkDir(h.root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return nil
				}
				for _, k := range h.keys {
					if k != "" && bytes.Contains(b, []byte(k)) {
						rel, _ := filepath.Rel(h.root, p)
						return fmt.Errorf("%s holds a visitor's key", rel)
					}
				}
				return nil
			})
		}},
		{Name: "runs-stay-with-their-starter", Check: func() error {
			// A visitor may act on a run only if they started it, and their
			// session lists only the runs they started.
			for _, s := range h.seen {
				if s.job != nil && (s.job.Stoppable || s.job.Resumable || s.job.Retryable) && h.starter[s.job.ID] != s.visitor {
					return fmt.Errorf("step %d: %s may act on %s, which %q started", s.step, s.visitor, h.label(s.job.ID), h.starter[s.job.ID])
				}
				if s.view != nil {
					for _, id := range s.view.Runs {
						if h.starter[id] != s.visitor {
							return fmt.Errorf("step %d: %s's session lists %s, which %q started", s.step, s.visitor, h.label(id), h.starter[id])
						}
					}
				}
			}
			return nil
		}},
		{Name: "keys-forgotten-on-restart", Check: func() error {
			// After a restart a session has keys only if its visitor entered
			// them again.
			for _, s := range h.seen {
				if s.view != nil && s.view.HasKeys && s.restarts > 0 && !s.keyed {
					return fmt.Errorf("step %d: %s has keys after a restart without entering them", s.step, s.visitor)
				}
			}
			return nil
		}},
	}
}

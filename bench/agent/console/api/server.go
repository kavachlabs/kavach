// Package api serves the benchmark console's HTTP API, documented in API.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// Config locates what the server reads and runs.
type Config struct {
	JobsDir   string // absolute
	TasksDir  string
	EnvFile   string // bench/agent/.env; passed to Harbor, never read
	Harbor    string // the harbor command; default "harbor"
	ModelsURL string // default OpenRouter's model list
	Web       fs.FS  // the web app, served at /; nil serves only the API
}

// Server is the API's http.Handler.
type Server struct {
	cfg    Config
	reader *harbor.Reader
	runner *harbor.Runner
	models *modelList
	events *hub
	mux    *http.ServeMux

	statusMu   sync.Mutex
	status     Status
	statusTime time.Time
}

// New returns a server for cfg.
func New(cfg Config) *Server {
	if cfg.Harbor == "" {
		cfg.Harbor = "harbor"
	}
	if cfg.ModelsURL == "" {
		cfg.ModelsURL = openRouterModels
	}
	runner := &harbor.Runner{Harbor: cfg.Harbor, JobsDir: cfg.JobsDir, TasksDir: cfg.TasksDir, EnvFile: cfg.EnvFile}
	reader := &harbor.Reader{Dir: cfg.JobsDir, Runs: runner}
	s := &Server{
		cfg:    cfg,
		reader: reader,
		runner: runner,
		models: &modelList{url: cfg.ModelsURL, client: &http.Client{Timeout: 20 * time.Second}},
		events: &hub{reader: reader, interval: time.Second},
		mux:    http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /api/status", s.getStatus)
	s.mux.HandleFunc("GET /api/tasks", s.getTasks)
	s.mux.HandleFunc("GET /api/models", s.getModels)
	s.mux.HandleFunc("GET /api/jobs", s.getJobs)
	s.mux.HandleFunc("POST /api/jobs", s.postJob)
	s.mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	s.mux.HandleFunc("GET /api/jobs/{id}/compare", s.getCompare)
	s.mux.HandleFunc("POST /api/jobs/{id}/stop", s.stopJob)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}", s.getTrial)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/steps", s.getSteps)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/patch", s.getPatch)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/grade", s.getGrade)
	s.mux.HandleFunc("GET /api/events", s.getEvents)
	if cfg.Web != nil {
		s.mux.Handle("GET /", http.FileServerFS(cfg.Web))
	}
	return s
}

// ServeHTTP serves the API. The server listens on 127.0.0.1 only, but a web
// page in the user's browser can still reach it, and POST /api/jobs spends
// money: requests must name a loopback host (against DNS rebinding), and
// writes from a browser must come from the same origin.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopback(r.Host) {
		writeError(w, http.StatusForbidden, "host %q is not a loopback address", r.Host)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				writeError(w, http.StatusForbidden, "cross-origin request from %s", o)
				return
			}
		}
	}
	s.mux.ServeHTTP(w, r)
}

func loopback(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Status is GET /api/status.
type Status struct {
	Docker   Check  `json:"docker"`
	Harbor   Check  `json:"harbor"`
	JobsDir  string `json:"jobs_dir"`
	TasksDir string `json:"tasks_dir"`
	Tasks    int    `json:"tasks"`    // benchmark tasks, each with its arms
	EnvFile  bool   `json:"env_file"` // bench/agent/.env exists
}

// Check is whether a tool works, with its version or why not.
type Check struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// statusTTL spares a polling frontend from running docker every second.
const statusTTL = 10 * time.Second

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if time.Since(s.statusTime) > statusTTL {
		st := Status{JobsDir: s.cfg.JobsDir, TasksDir: s.cfg.TasksDir}
		st.Docker = check(r.Context(), "docker", "version", "--format", "{{.Server.Version}}")
		st.Harbor = check(r.Context(), s.cfg.Harbor, "--version")
		tasks, _ := harbor.FindTasks(s.cfg.TasksDir)
		st.Tasks = len(tasks)
		_, err := os.Stat(s.cfg.EnvFile)
		st.EnvFile = s.cfg.EnvFile != "" && err == nil
		s.status, s.statusTime = st, time.Now()
	}
	writeJSON(w, http.StatusOK, s.status)
}

func check(ctx context.Context, name string, args ...string) Check {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return Check{Error: text}
	}
	return Check{OK: true, Version: text}
}

func (s *Server) getTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := harbor.FindTasks(s.cfg.TasksDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) getModels(w http.ResponseWriter, r *http.Request) {
	list, err := s.models.get(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "%v", err)
		return
	}
	q := strings.ToLower(r.URL.Query().Get("q"))
	out := []Model{}
	for _, m := range list {
		if strings.Contains(strings.ToLower(m.ID), q) || strings.Contains(strings.ToLower(m.Name), q) {
			out = append(out, m)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.reader.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	for i := range jobs {
		jobs[i].Trials = nil
	}
	if jobs == nil {
		jobs = []harbor.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	s.writeJob(w, http.StatusOK, r.PathValue("id"))
}

// writeJob writes a job with its trials.
func (s *Server) writeJob(w http.ResponseWriter, code int, id string) {
	j, err := s.reader.Job(id)
	if err != nil {
		writeReadError(w, err)
		return
	}
	// harbor.Job omits empty trials, for the job list; here they are always
	// sent. The outer field shadows the embedded one.
	type withTrials struct {
		harbor.Job
		Trials []harbor.Trial `json:"trials"`
	}
	out := withTrials{j, j.Trials}
	if out.Trials == nil {
		out.Trials = []harbor.Trial{}
	}
	writeJSON(w, code, out)
}

func (s *Server) getCompare(w http.ResponseWriter, r *http.Request) {
	j, err := s.reader.Job(r.PathValue("id"))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, harbor.Compare(j.Trials))
}

func (s *Server) getTrial(w http.ResponseWriter, r *http.Request) {
	t, err := s.reader.Trial(r.PathValue("job"), r.PathValue("trial"))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) getSteps(w http.ResponseWriter, r *http.Request) {
	steps, err := s.reader.Steps(r.PathValue("job"), r.PathValue("trial"))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, steps)
}

func (s *Server) getPatch(w http.ResponseWriter, r *http.Request) {
	patch, err := s.reader.Patch(r.PathValue("job"), r.PathValue("trial"))
	if err != nil {
		writeReadError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(patch)
}

func (s *Server) getGrade(w http.ResponseWriter, r *http.Request) {
	g, err := s.reader.Grade(r.PathValue("job"), r.PathValue("trial"))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) postJob(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	var req harbor.RunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	id, err := s.runner.Start(req)
	switch {
	case errors.Is(err, harbor.ErrInvalid):
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	case errors.Is(err, harbor.ErrExists):
		writeError(w, http.StatusConflict, "%v", err)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "starting harbor: %v", err)
		return
	}
	s.writeJob(w, http.StatusAccepted, id)
}

func (s *Server) stopJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.runner.Stop(id); err != nil {
		writeError(w, http.StatusConflict, "%s: %v", id, err)
		return
	}
	s.writeJob(w, http.StatusAccepted, id)
}

func writeReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, harbor.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "%v", err)
}

func writeError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

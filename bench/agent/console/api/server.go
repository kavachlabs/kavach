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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// Config locates what the server reads and runs.
type Config struct {
	JobsDir  string // absolute
	TasksDir string
	EnvFile  string // bench/agent/.env; passed to Harbor, never read
	Harbor   string // the harbor command; default "harbor"
	Root     string // the kavach repository; when set, runs use its OpenCode agent
	// JobsWarning, when set, says why runs in JobsDir can't be graded; the
	// consoles show it and refuse to start runs.
	JobsWarning string
	ModelsURL   string // default OpenRouter's model list
	Web         fs.FS  // the web app, served at /; nil serves only the API
	// Docker and CacheDir let the invariants view show the real source files
	// of a trial; without them it shows the patches.
	Docker   harbor.Docker
	CacheDir string
	// Trial, when set, makes the server a public trial; see trial.go.
	Trial *TrialConfig
}

// Server is the API's http.Handler.
type Server struct {
	cfg    Config
	reader *harbor.Reader
	runner *harbor.Runner
	syncer *harbor.Syncer
	models *modelList
	events *hub
	mux    *http.ServeMux
	trial  *trial // nil unless Config.Trial is set

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
	if cfg.Trial != nil {
		// Reading real source files starts containers for any visitor.
		cfg.Docker = nil
	}
	runner := &harbor.Runner{Harbor: cfg.Harbor, JobsDir: cfg.JobsDir, TasksDir: cfg.TasksDir, EnvFile: cfg.EnvFile, Root: cfg.Root}
	reader := &harbor.Reader{Dir: cfg.JobsDir, Runs: runner, Docker: cfg.Docker, CacheDir: cfg.CacheDir}
	syncer := &harbor.Syncer{Runner: runner, Reader: reader}
	reader.Sync, runner.OnExit, runner.OnStart = syncer, syncer.Queue, syncer.Cancel
	s := &Server{
		cfg:    cfg,
		reader: reader,
		runner: runner,
		syncer: syncer,
		models: &modelList{url: cfg.ModelsURL, client: &http.Client{Timeout: 20 * time.Second}},
		events: &hub{reader: reader, interval: time.Second},
		mux:    http.NewServeMux(),
	}
	if cfg.Trial != nil {
		s.trial = newTrial(*cfg.Trial, runner, cfg.JobsDir, cfg.TasksDir)
		s.events.visible = s.trial.visible
		s.trialRoutes()
		return s
	}
	s.mux.HandleFunc("GET /api/status", s.getStatus)
	s.mux.HandleFunc("GET /api/settings", s.getSettings)
	s.mux.HandleFunc("POST /api/settings", s.postSettings)
	s.mux.HandleFunc("GET /api/tasks", s.getTasks)
	s.mux.HandleFunc("GET /api/tasks/{task}/environment", s.getTaskEnvironment)
	s.mux.HandleFunc("GET /api/models", s.getModels)
	s.mux.HandleFunc("GET /api/jobs", s.getJobs)
	s.mux.HandleFunc("POST /api/jobs", s.postJob)
	s.mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	s.mux.HandleFunc("GET /api/jobs/{id}/compare", s.getCompare)
	s.mux.HandleFunc("POST /api/jobs/{id}/stop", s.stopJob)
	s.mux.HandleFunc("POST /api/jobs/{id}/pause", s.pauseJob)
	s.mux.HandleFunc("POST /api/jobs/{id}/resume", s.resumeJob)
	s.mux.HandleFunc("POST /api/jobs/{id}/sync", s.syncJob)
	s.mux.HandleFunc("POST /api/trials/{job}/{trial}/retry", s.retryTrial)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}", s.getTrial)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/steps", s.getSteps)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/patch", s.getPatch)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/grade", s.getGrade)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/invariants", s.getInvariants)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/invariants/source", s.getInvariantSource)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/invariants/file", s.getInvariantFile)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/incident", s.getIncident)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/variants", s.getVariants)
	s.mux.HandleFunc("GET /api/trials/{job}/{trial}/variants/{round}/{id}", s.getVariant)
	s.mux.HandleFunc("GET /api/events", s.getEvents)
	if cfg.Web != nil {
		s.mux.Handle("GET /", http.FileServerFS(cfg.Web))
	}
	return s
}

// Syncer is the server's R2 backup of finished jobs.
func (s *Server) Syncer() *harbor.Syncer { return s.syncer }

// StartSync backs up every finished job that changed since its last sync,
// then keeps checking. A server that only reads (bench export) does not.
func (s *Server) StartSync() { s.syncer.StartSweeps() }

// ServeHTTP serves the API. The server listens on 127.0.0.1 only, but a web
// page in the user's browser can still reach it, and POST /api/jobs spends
// money: requests must name a loopback host (against DNS rebinding), and
// writes from a browser must come from the same origin.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.trial != nil {
		s.trial.serve(s, w, r)
		return
	}
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
	Root     string `json:"root,omitempty"`
	// JobsWarning says why runs in JobsDir may fail to grade.
	JobsWarning string `json:"jobs_warning,omitempty"`
	// Environments says which remote providers have their keys; names only.
	Environments []EnvironmentStatus `json:"environments"`
	// Sync is the backup of finished runs to R2.
	Sync SyncStatus `json:"sync"`
}

// SyncStatus says whether runs are backed up to R2 and with what; the keys
// are masked as in /api/settings.
type SyncStatus struct {
	Enabled bool `json:"enabled"`
	// Reason says why it is off.
	Reason string    `json:"reason,omitempty"`
	Bucket string    `json:"bucket"`
	Keys   []SyncKey `json:"keys"`
}

// SyncKey is one R2 credential.
type SyncKey struct {
	Name   string `json:"name"`
	Set    bool   `json:"set"`
	Masked string `json:"masked"`
}

// EnvironmentStatus is whether a run on a remote provider can start: its
// credentials are present and the Harbor install has its extra.
type EnvironmentStatus struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	// Need describes the credentials the provider accepts.
	Need string `json:"need"`
	// Via names the file that authenticates when no variable does.
	Via string `json:"via,omitempty"`
	// Problem says why Ready is false.
	Problem string      `json:"problem,omitempty"`
	Keys    []KeyStatus `json:"keys"`
}

// KeyStatus is one variable a provider needs and whether it is set.
type KeyStatus struct {
	Name string `json:"name"`
	Set  bool   `json:"set"`
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
		st := Status{JobsDir: s.cfg.JobsDir, TasksDir: s.cfg.TasksDir, Root: s.cfg.Root, JobsWarning: s.cfg.JobsWarning}
		st.Docker = check(r.Context(), "docker", "version", "--format", "{{.Server.Version}}")
		st.Harbor = check(r.Context(), s.cfg.Harbor, "--version")
		tasks, _ := harbor.FindTasks(s.cfg.TasksDir)
		st.Tasks = len(tasks)
		_, err := os.Stat(s.cfg.EnvFile)
		st.EnvFile = s.cfg.EnvFile != "" && err == nil
		s.status, s.statusTime = st, time.Now()
	}
	st := s.status
	// Not cached: the keys change when settings are saved.
	st.Environments = s.environments()
	st.Sync = s.syncStatus()
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) environments() []EnvironmentStatus {
	out := []EnvironmentStatus{}
	for _, p := range harbor.Providers {
		_, via := s.runner.Credentials(p)
		problem := s.runner.Problem(p)
		e := EnvironmentStatus{Name: p.Name, Ready: problem == "", Need: p.Need(), Via: via, Problem: problem, Keys: []KeyStatus{}}
		for _, k := range p.Keys() {
			e.Keys = append(e.Keys, KeyStatus{Name: k, Set: s.runner.KeySet(k)})
		}
		out = append(out, e)
	}
	return out
}

func (s *Server) syncStatus() SyncStatus {
	cfg := s.runner.R2()
	st := SyncStatus{Bucket: cfg.Bucket, Keys: []SyncKey{}}
	if reason := s.syncer.Off(); reason != "" {
		st.Reason = reason
	} else {
		st.Enabled = true
	}
	for _, k := range []struct{ name, val string }{
		{"R2_ACCOUNT_ID", cfg.AccountID}, {"R2_ACCESS_KEY_ID", cfg.AccessKeyID}, {"R2_SECRET_ACCESS_KEY", cfg.SecretAccessKey},
	} {
		st.Keys = append(st.Keys, SyncKey{k.name, k.val != "", maskSecret(k.name, k.val)})
	}
	return st
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

func (s *Server) getTaskEnvironment(w http.ResponseWriter, r *http.Request) {
	env, err := harbor.TaskEnvironment(s.cfg.TasksDir, r.PathValue("task"))
	if errors.Is(err, harbor.ErrNoTask) {
		writeError(w, http.StatusNotFound, "no task named %q", r.PathValue("task"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, env)
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
	s.writeJob(w, r, http.StatusOK, r.PathValue("id"))
}

// writeJob writes a job with its trials.
func (s *Server) writeJob(w http.ResponseWriter, r *http.Request, code int, id string) {
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
	if s.trial != nil {
		v := s.trial.viewer(r)
		s.trial.shape(&j, v, s.trial.ownedBy(v, id))
		for i := range j.Trials {
			s.trial.shapeTrial(&j.Trials[i])
		}
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

// getInvariants serves what the trial's service declares as invariants.
// names, comma-separated, are invariants its contract checks named.
func (s *Server) getInvariants(w http.ResponseWriter, r *http.Request) {
	var names []string
	if q := r.URL.Query().Get("names"); q != "" {
		names = strings.Split(q, ",")
	}
	job, trial := r.PathValue("job"), r.PathValue("trial")
	inv, err := s.reader.TrialInvariants(s.cfg.TasksDir, job, trial, names)
	if err == nil {
		err = s.reader.RealSource(s.cfg.TasksDir, job, trial, inv)
	}
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

// getInvariantSource serves the text of one of the patches the invariants
// are read from: sources[i] of getInvariants. No other file can be read.
func (s *Server) getInvariantSource(w http.ResponseWriter, r *http.Request) {
	i, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil || i < 0 {
		writeError(w, http.StatusBadRequest, "i must be a non-negative integer")
		return
	}
	text, err := s.reader.InvariantSource(s.cfg.TasksDir, r.PathValue("job"), r.PathValue("trial"), i)
	if err != nil {
		writeReadError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(text)
}

// getInvariantFile serves a real source file of the trial, as fetched by
// getInvariants: path is one of its files. Only files the trial's patches
// touch can be read.
func (s *Server) getInvariantFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	text, err := s.reader.RealFile(s.cfg.TasksDir, r.PathValue("job"), r.PathValue("trial"), path)
	if err != nil {
		writeReadError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(text)
}

func (s *Server) getIncident(w http.ResponseWriter, r *http.Request) {
	inc, err := s.reader.TrialIncident(r.PathValue("job"), r.PathValue("trial"))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

func (s *Server) getVariants(w http.ResponseWriter, r *http.Request) {
	v, err := s.reader.TrialVariants(r.PathValue("job"), r.PathValue("trial"))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getVariant(w http.ResponseWriter, r *http.Request) {
	round, err1 := strconv.Atoi(r.PathValue("round"))
	id, err2 := strconv.Atoi(r.PathValue("id"))
	if err1 != nil || err2 != nil || round < 0 || id < 0 {
		writeError(w, http.StatusBadRequest, "round and id must be non-negative integers")
		return
	}
	v, err := s.reader.TrialVariant(r.PathValue("job"), r.PathValue("trial"), round, id)
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
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
	s.writeJob(w, r, http.StatusAccepted, id)
}

func (s *Server) stopJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.runner.Stop(id); err != nil {
		writeError(w, http.StatusConflict, "%s: %v", id, err)
		return
	}
	s.writeJob(w, r, http.StatusAccepted, id)
}

func (s *Server) syncJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if reason := s.syncer.Off(); reason != "" {
		writeError(w, http.StatusConflict, "R2 sync is off: %s", reason)
		return
	}
	switch err := s.syncer.Check(id); {
	case errors.Is(err, harbor.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, harbor.ErrSyncBusy):
		writeError(w, http.StatusConflict, "%s is running or paused; it syncs when its Harbor has finished", id)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	s.syncer.Queue(id)
	s.writeJob(w, r, http.StatusAccepted, id)
}

func (s *Server) pauseJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.runner.Pause(id); err != nil {
		writeError(w, http.StatusConflict, "%s: %v", id, err)
		return
	}
	s.writeJob(w, r, http.StatusAccepted, id)
}

func (s *Server) resumeJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.runner.Resume(id)
	switch {
	case errors.Is(err, harbor.ErrNotStopped):
		writeError(w, http.StatusConflict, "%s: %v", id, err)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "resuming harbor: %v", err)
		return
	}
	s.writeJob(w, r, http.StatusAccepted, id)
}

func (s *Server) retryTrial(w http.ResponseWriter, r *http.Request) {
	id, trial := r.PathValue("job"), r.PathValue("trial")
	err := s.runner.Retry(id, trial)
	switch {
	case errors.Is(err, harbor.ErrNotFound):
		writeError(w, http.StatusNotFound, "%s/%s: no such attempt in a run bench started", id, trial)
		return
	case errors.Is(err, harbor.ErrNotStopped):
		writeError(w, http.StatusConflict, "%s is still running; retry the attempt once it ends", id)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "retrying %s: %v", trial, err)
		return
	}
	s.writeJob(w, r, http.StatusAccepted, id)
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

// KeyEntry describes one API key in bench/agent/.env (never returning the raw secret).
type KeyEntry struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Hint   string `json:"hint"`
	Set    bool   `json:"set"`
	Masked string `json:"masked"`
}

// SettingsResponse is returned by GET and POST /api/settings.
type SettingsResponse struct {
	EnvFile string     `json:"env_file"`
	Exists  bool       `json:"exists"`
	Keys    []KeyEntry `json:"keys"`
}

var knownKeys = []struct {
	name, label, hint string
}{
	{"OPENROUTER_API_KEY", "OpenRouter API Key", "Used by the OpenCode agent for frontier & open-weight models"},
	{"ANTHROPIC_API_KEY", "Anthropic API Key", "Direct Anthropic API access for Claude models"},
	{"OPENAI_API_KEY", "OpenAI API Key", "Direct OpenAI API access for GPT & o-series models"},
	{"NVIDIA_API_KEY", "NVIDIA NIM API Key", "Hosted Nemotron endpoints on build.nvidia.com"},
	{"ZAI_API_KEY", "Z.ai API Key", "Direct Z.ai API access for GLM models"},
	{"DAYTONA_API_KEY", "Daytona API Key", "Runs the trials on Daytona instead of local Docker"},
	{"MODAL_TOKEN_ID", "Modal Token ID", "Runs the trials on Modal; needs the token secret too"},
	{"MODAL_TOKEN_SECRET", "Modal Token Secret", "Runs the trials on Modal; needs the token ID too"},
	{"DAYTONA_JWT_TOKEN", "Daytona JWT Token", "Instead of the API key; needs the organization ID too"},
	{"DAYTONA_ORGANIZATION_ID", "Daytona Organization ID", "Used with the Daytona JWT token"},
	{"VERCEL_TOKEN", "Vercel Token", "Runs the trials on Vercel Sandbox"},
	{"R2_ACCOUNT_ID", "R2 Account ID", "Backs finished runs up to Cloudflare R2; needs the two R2 keys too"},
	{"R2_ACCESS_KEY_ID", "R2 Access Key ID", "An R2 API token with Object Read & Write on the bucket"},
	{"R2_SECRET_ACCESS_KEY", "R2 Secret Access Key", "The secret of that token"},
}

func (s *Server) envFilePath() string {
	if s.cfg.EnvFile != "" {
		return s.cfg.EnvFile
	}
	if s.cfg.Root != "" {
		return filepath.Join(s.cfg.Root, "bench", "agent", ".env")
	}
	if s.cfg.JobsDir != "" {
		return filepath.Join(s.cfg.JobsDir, ".env")
	}
	return ""
}

// maskSecret keeps a key's recognisable prefix and last four characters. A
// provider's key has no prefix known to be fixed, so only its last four show.
func maskSecret(name, val string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return ""
	}
	if len(val) <= 8 {
		return "••••••••"
	}
	for _, p := range harbor.Providers {
		if slices.Contains(p.Keys(), name) {
			return "••••" + val[len(val)-4:]
		}
	}
	if strings.HasPrefix(name, "R2_") {
		return "••••" + val[len(val)-4:]
	}
	prefix := val[:min(8, len(val)-4)]
	suffix := val[len(val)-4:]
	return prefix + "••••" + suffix
}

func (s *Server) buildSettingsResponse() SettingsResponse {
	path := s.envFilePath()
	vals, exists := harbor.ReadEnv(path)
	seen := map[string]bool{}
	keys := make([]KeyEntry, 0, len(knownKeys))
	for _, k := range knownKeys {
		seen[k.name] = true
		v := vals[k.name]
		keys = append(keys, KeyEntry{
			Name:   k.name,
			Label:  k.label,
			Hint:   k.hint,
			Set:    v != "",
			Masked: maskSecret(k.name, v),
		})
	}
	for name, v := range vals {
		if seen[name] || !strings.HasSuffix(name, "_KEY") {
			continue
		}
		keys = append(keys, KeyEntry{
			Name:   name,
			Label:  name,
			Hint:   "Custom environment key in .env",
			Set:    v != "",
			Masked: maskSecret(name, v),
		})
	}
	return SettingsResponse{EnvFile: path, Exists: exists, Keys: keys}
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildSettingsResponse())
}

func isValidEnvKey(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, c := range s {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	var req struct {
		Keys map[string]string `json:"keys"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	for k := range req.Keys {
		if !isValidEnvKey(k) {
			writeError(w, http.StatusBadRequest, "invalid key name %q", k)
			return
		}
	}
	path := s.envFilePath()
	if path == "" {
		writeError(w, http.StatusInternalServerError, "no .env path configured")
		return
	}
	if err := writeEnvKeys(path, req.Keys); err != nil {
		writeError(w, http.StatusInternalServerError, "saving .env: %v", err)
		return
	}
	s.statusMu.Lock()
	s.statusTime = time.Time{}
	s.statusMu.Unlock()
	writeJSON(w, http.StatusOK, s.buildSettingsResponse())
}

func writeEnvKeys(path string, updates map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	} else if os.IsNotExist(err) {
		lines = []string{
			"# Copy to bench/agent/.env (gitignored) and fill in the keys you use.",
			"# run.sh and bench load it automatically.",
			"",
		}
	} else {
		return err
	}

	written := map[string]bool{}
	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		clean := strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
		k, _, ok := strings.Cut(clean, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if newVal, has := updates[k]; has {
			sanitized := strings.NewReplacer("\r", "", "\n", "").Replace(strings.TrimSpace(newVal))
			lines[i] = k + "=" + sanitized
			written[k] = true
		}
	}
	for _, kk := range knownKeys {
		if newVal, has := updates[kk.name]; has && !written[kk.name] {
			sanitized := strings.NewReplacer("\r", "", "\n", "").Replace(strings.TrimSpace(newVal))
			lines = append(lines, kk.name+"="+sanitized)
			written[kk.name] = true
		}
	}
	for k, newVal := range updates {
		if !written[k] {
			sanitized := strings.NewReplacer("\r", "", "\n", "").Replace(strings.TrimSpace(newVal))
			lines = append(lines, k+"="+sanitized)
		}
	}
	content := strings.Join(lines, "\n") + "\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

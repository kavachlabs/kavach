package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrInvalid wraps a run request that cannot be started as given.
	ErrInvalid = errors.New("invalid run")
	// ErrExists is returned for a job name that is already taken.
	ErrExists = errors.New("job already exists")
	// ErrNotRunning is returned when stopping a job this process is not running.
	ErrNotRunning = errors.New("job is not running here")
)

// RunRequest describes a Harbor run.
type RunRequest struct {
	// Tasks are Harbor task directories: absolute, or relative to the
	// runner's TasksDir (redis-10068-kavach).
	Tasks []string `json:"tasks"`
	// Models are the agent's model names (openrouter/z-ai/glm-5.3); none for
	// agents without a model, such as oracle and nop.
	Models []string `json:"models"`
	// Attempts per model and task (Harbor's n_attempts). Default 1.
	Attempts int `json:"attempts"`
	// Agent is a Harbor agent name or a custom agent's import path
	// (module.path:ClassName). Default opencode.
	Agent string `json:"agent"`
	// TimeoutSec overrides the tasks' agent timeout when positive.
	TimeoutSec float64 `json:"timeout_sec"`
	// Concurrent is how many trials run at once. Default 1.
	Concurrent int `json:"concurrent"`
	// Name is the job's name and folder; default the time, as Harbor names it.
	Name string `json:"name"`
}

// Runner starts Harbor runs and tracks the processes it started.
//
// Each run is its own process group, so the console's Ctrl-C does not reach
// Harbor and runs outlive the console. shortcut: a run started by an earlier
// console cannot be stopped from this one; record its pid if that matters.
type Runner struct {
	Harbor   string // the harbor command
	JobsDir  string // absolute: Harbor mounts trial folders into containers
	TasksDir string
	EnvFile  string // given to Harbor as --env-file if it exists; never read here

	mu   sync.Mutex
	runs map[string]*run
}

type run struct {
	cmd     *exec.Cmd
	started time.Time
	done    chan struct{}
	out     *tail
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Start launches `harbor run` for req and returns the job's name.
func (r *Runner) Start(req RunRequest) (string, error) {
	cfg, err := r.jobConfig(&req)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs[req.Name] != nil || exists(filepath.Join(r.JobsDir, req.Name)) {
		return "", fmt.Errorf("%w: %s", ErrExists, req.Name)
	}
	f, err := os.CreateTemp("", "kavach-bench-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	args := []string{"run", "-c", f.Name(), "-y"}
	if r.EnvFile != "" && exists(r.EnvFile) {
		args = append(args, "--env-file", r.EnvFile)
	}
	cmd := exec.Command(r.Harbor, args...)
	out := &tail{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	rn := &run{cmd: cmd, started: time.Now(), done: make(chan struct{}), out: out}
	if r.runs == nil {
		r.runs = map[string]*run{}
	}
	r.runs[req.Name] = rn
	go func() {
		err := cmd.Wait()
		if err != nil {
			fmt.Fprintf(out, "\nharbor: %v\n", err)
		}
		os.Remove(f.Name())
		close(rn.done)
	}()
	return req.Name, nil
}

// jobConfig validates req, fills in its defaults and returns Harbor's
// JobConfig for it.
func (r *Runner) jobConfig(req *RunRequest) (map[string]any, error) {
	if len(req.Tasks) == 0 {
		return nil, fmt.Errorf("%w: no tasks", ErrInvalid)
	}
	var tasks []map[string]string
	for _, t := range req.Tasks {
		path := t
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.TasksDir, t)
		}
		if !exists(filepath.Join(path, "task.toml")) {
			return nil, fmt.Errorf("%w: %s is not a Harbor task (no task.toml)", ErrInvalid, t)
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, map[string]string{"path": path})
	}
	if req.Attempts == 0 {
		req.Attempts = 1
	}
	if req.Concurrent == 0 {
		req.Concurrent = 1
	}
	if req.Attempts < 1 || req.Attempts > 100 || req.Concurrent < 1 || req.Concurrent > 32 {
		return nil, fmt.Errorf("%w: attempts must be 1-100 and concurrent 1-32", ErrInvalid)
	}
	if req.TimeoutSec < 0 {
		return nil, fmt.Errorf("%w: negative timeout", ErrInvalid)
	}
	if req.Agent == "" {
		req.Agent = "opencode"
	}
	if req.Name == "" {
		req.Name = time.Now().Format("2006-01-02__15-04-05")
	}
	if !namePattern.MatchString(req.Name) {
		return nil, fmt.Errorf("%w: job name %q", ErrInvalid, req.Name)
	}
	models := req.Models
	if len(models) == 0 {
		models = []string{""}
	}
	var agents []map[string]any
	for _, m := range models {
		a := map[string]any{"name": req.Agent}
		if strings.Contains(req.Agent, ":") {
			a = map[string]any{"import_path": req.Agent}
		}
		if m = strings.TrimSpace(m); m != "" {
			a["model_name"] = m
		}
		if req.TimeoutSec > 0 {
			a["override_timeout_sec"] = req.TimeoutSec
		}
		agents = append(agents, a)
	}
	return map[string]any{
		"job_name":            req.Name,
		"jobs_dir":            r.JobsDir,
		"n_attempts":          req.Attempts,
		"n_concurrent_trials": req.Concurrent,
		"quiet":               true,
		"agents":              agents,
		"tasks":               tasks,
	}, nil
}

// Stop interrupts a run this process started. Harbor treats SIGINT like
// Ctrl-C: it cancels the trials, removes their containers and exits; the
// cancelled trials end with a CancelledError and the job never finishes.
// Only Harbor gets the signal, not its docker children, which it needs for
// the cleanup.
func (r *Runner) Stop(id string) error {
	rn := r.get(id)
	if rn == nil {
		return ErrNotRunning
	}
	select {
	case <-rn.done:
		return ErrNotRunning
	default:
	}
	return rn.cmd.Process.Signal(os.Interrupt)
}

// Wait blocks until a run this process started has exited.
func (r *Runner) Wait(id string) {
	if rn := r.get(id); rn != nil {
		<-rn.done
	}
}

func (r *Runner) get(id string) *run {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[id]
}

func (r *Runner) ids() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id := range r.runs {
		ids = append(ids, id)
	}
	return ids
}

func (r *Runner) launched(id string) bool { return r.get(id) != nil }

// state says whether this process started the job and whether Harbor is
// still running it, with the end of Harbor's output.
func (r *Runner) state(id string) (launched, alive bool, out string) {
	rn := r.get(id)
	if rn == nil {
		return false, false, ""
	}
	select {
	case <-rn.done:
	default:
		alive = true
	}
	return true, alive, strings.TrimSpace(rn.out.String())
}

func (r *Runner) started(id string) *time.Time {
	if rn := r.get(id); rn != nil {
		return &rn.started
	}
	return nil
}

// tail keeps the last tailSize bytes written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

const tailSize = 4096

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - tailSize; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

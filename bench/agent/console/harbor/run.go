package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	// ErrNotRunning is returned when stopping a job that bench did not start
	// or whose Harbor has exited.
	ErrNotRunning = errors.New("job is not running")
	// ErrNotStopped is returned when resuming a job that bench did not
	// start, that is still running or that has finished, or when retrying
	// an attempt of a job that is still running.
	ErrNotStopped = errors.New("job is not paused or stopped")
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
	// Retries is how many times Harbor runs an attempt again when it ends
	// with an error (a provider or container failure), 0-5. Harbor deletes
	// the failed attempt's folder first, and does not retry timeouts, usage
	// limits or refusals, nor an attempt whose tests ran and failed.
	// Default 0.
	Retries int `json:"retries"`
	// Name is the job's name and folder; default three random words
	// (brisk-cobalt-heron).
	Name string `json:"name"`
	// Environment is where the trials execute: docker (default, local),
	// daytona, modal or vercel. The last three need their keys in the env
	// file or the process environment.
	Environment string `json:"environment"`
}

// ReadEnv parses a dotenv file; ok is false when it cannot be read.
func ReadEnv(path string) (vals map[string]string, ok bool) {
	vals = map[string]string{}
	if path == "" {
		return vals, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return vals, false
	}
	for _, raw := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			vals[k] = v
		}
	}
	return vals, true
}

// Runner starts Harbor runs and tracks them.
//
// Runs outlive the console. Each is its own process group, so the console's
// Ctrl-C does not reach Harbor, and its output goes to a file rather than a
// pipe, which would kill Harbor on its next write once the console is gone.
// For every run, <jobs>/.bench/ keeps <id>.json (the JobConfig given to
// Harbor), <id>.log (Harbor's output) and <id>.pid, so a later console can
// tell whether Harbor is still running and stop it.
type Runner struct {
	Harbor   string // the harbor command
	JobsDir  string // absolute: Harbor mounts trial folders into containers
	TasksDir string
	EnvFile  string // given to Harbor as --env-file if it exists; read only to tell which provider keys are set, never returned
	// Extra reports whether the Harbor install can run a provider; nil
	// asks Harbor's own Python (see Installed).
	Extra func(Provider) error
	// Root is the kavach repository. When set, Harbor runs there with it on
	// PYTHONPATH, and the opencode agent is LiveAgent rather than Harbor's
	// own: the model key stays out of the task container and each attempt
	// can be watched in OpenCode.
	Root string

	mu   sync.Mutex
	runs map[string]*run

	extraMu sync.Mutex
	extras  map[string]extraResult
}

// LiveAgent is the import path of the repository's OpenCode agent for Harbor.
const LiveAgent = "bench.agent.harbor.agents:KavachOpenCode"

// run is a Harbor process this Runner started and must reap.
type run struct {
	cmd  *exec.Cmd
	done chan struct{}
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
	if exists(filepath.Join(r.JobsDir, req.Name)) || exists(r.file(req.Name, ".json")) {
		return "", fmt.Errorf("%w: %s", ErrExists, req.Name)
	}
	if err := os.MkdirAll(r.file("", ""), 0o755); err != nil {
		return "", err
	}
	cfgFile := r.file(req.Name, ".json")
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(cfgFile, b, 0o644); err != nil {
		return "", err
	}
	args := []string{"run", "-c", cfgFile, "-y"}
	if r.EnvFile != "" && exists(r.EnvFile) {
		args = append(args, "--env-file", r.EnvFile)
	}
	if err := r.spawn(req.Name, exec.Command(r.Harbor, args...)); err != nil {
		return "", err
	}
	return req.Name, nil
}

// spawn starts cmd as the job's Harbor, appending its output to the job's
// log, and tracks it. r.mu must be held.
func (r *Runner) spawn(id string, cmd *exec.Cmd) error {
	log, err := os.OpenFile(r.file(id, ".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if r.Root != "" {
		cmd.Dir = r.Root
		cmd.Env = append(os.Environ(), "PYTHONPATH="+r.Root)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(log, "harbor: %v\n", err)
		return err
	}
	if err := os.WriteFile(r.file(id, ".pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		cmd.Process.Signal(os.Interrupt)
		return err
	}
	rn := &run{cmd: cmd, done: make(chan struct{})}
	if r.runs == nil {
		r.runs = map[string]*run{}
	}
	r.runs[id] = rn
	go func() {
		if err := cmd.Wait(); err != nil {
			if f, ferr := os.OpenFile(r.file(id, ".log"), os.O_WRONLY|os.O_APPEND, 0); ferr == nil {
				fmt.Fprintf(f, "\nharbor: %v\n", err)
				f.Close()
			}
		}
		close(rn.done)
	}()
	return nil
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
	if req.Retries < 0 || req.Retries > 5 {
		return nil, fmt.Errorf("%w: retries must be 0-5", ErrInvalid)
	}
	if req.TimeoutSec < 0 {
		return nil, fmt.Errorf("%w: negative timeout", ErrInvalid)
	}
	if req.Agent == "" {
		req.Agent = "opencode"
	}
	if req.Name == "" {
		req.Name = r.newName()
	}
	if !namePattern.MatchString(req.Name) {
		return nil, fmt.Errorf("%w: job name %q", ErrInvalid, req.Name)
	}
	var provider *Provider
	for i, p := range Providers {
		if p.Name == req.Environment {
			provider = &Providers[i]
		}
	}
	if provider == nil && req.Environment != "" && req.Environment != "docker" {
		return nil, fmt.Errorf("%w: environment %q is not docker, daytona, modal or vercel", ErrInvalid, req.Environment)
	}
	if provider != nil {
		if problem := r.Problem(*provider); problem != "" {
			return nil, fmt.Errorf("%w: %s", ErrInvalid, problem)
		}
	}
	models := req.Models
	if len(models) == 0 {
		models = []string{""}
	}
	agent := req.Agent
	if agent == "opencode" && r.Root != "" {
		agent = LiveAgent
	}
	var agents []map[string]any
	for _, m := range models {
		a := map[string]any{"name": agent}
		if strings.Contains(agent, ":") {
			a = map[string]any{"import_path": agent}
		}
		if m = strings.TrimSpace(m); m != "" {
			a["model_name"] = m
		}
		if req.TimeoutSec > 0 {
			a["override_timeout_sec"] = req.TimeoutSec
		}
		agents = append(agents, a)
	}
	cfg := map[string]any{
		"job_name":            req.Name,
		"jobs_dir":            r.JobsDir,
		"n_attempts":          req.Attempts,
		"n_concurrent_trials": req.Concurrent,
		"quiet":               true,
		"retry":               map[string]any{"max_retries": req.Retries},
		"agents":              agents,
		"tasks":               tasks,
	}
	if provider != nil {
		env := map[string]any{"type": provider.Name}
		if provider.Name == "modal" {
			// Modal's default runtime puts every Compose service on the
			// host network, which breaks tasks with their own networks;
			// the VM runtime runs a real Docker daemon. The factory passes
			// kwargs to ModalEnvironment's constructor.
			env["kwargs"] = map[string]any{"modal_vm_runtime": true}
		}
		cfg["environment"] = env
	}
	return cfg, nil
}

// Stop interrupts a run bench started, by this console or an earlier one.
// Harbor treats SIGINT like Ctrl-C: it cancels the trials, removes their
// containers and exits; the cancelled trials end with a CancelledError and
// the job never finishes. Only Harbor gets the signal, not its docker
// children, which it needs for the cleanup.
func (r *Runner) Stop(id string) error {
	if !validName(id) {
		return ErrNotRunning
	}
	if rn := r.get(id); rn != nil {
		select {
		case <-rn.done:
			return ErrNotRunning
		default:
			return rn.cmd.Process.Signal(os.Interrupt)
		}
	}
	pid, ok := r.pid(id)
	if !ok {
		return ErrNotRunning
	}
	return syscall.Kill(pid, syscall.SIGINT)
}

// Pause stops a run so that Resume can continue it. Harbor has no pause of
// its own: the run is stopped as by Stop, and marked paused. Freezing the
// containers instead would leave the agent's timeout running and the
// model's connections to time out.
func (r *Runner) Pause(id string) error {
	if _, alive, _ := r.state(id); !alive {
		return ErrNotRunning
	}
	if err := os.WriteFile(r.file(id, ".paused"), nil, 0o644); err != nil {
		return err
	}
	if err := r.Stop(id); err != nil {
		os.Remove(r.file(id, ".paused"))
		return err
	}
	return nil
}

// Resume continues a paused or stopped run with `harbor jobs resume`, which
// keeps the finished attempts and runs the rest, including those cancelled
// mid-way, from the start. A run stopped before Harbor wrote its job folder
// is started again instead.
func (r *Runner) Resume(id string) error {
	if !r.launched(id) {
		return ErrNotStopped
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, alive, _ := r.stateLocked(id); alive {
		return ErrNotStopped
	}
	var res jobResult
	if readJSON(filepath.Join(r.JobsDir, id, "result.json"), &res) == nil && res.FinishedAt != nil {
		return ErrNotStopped
	}
	return r.resumeLocked(id)
}

// Retry runs one attempt of a finished, stopped or paused run again. The
// attempt's folder moves to <jobs>/.bench/retried/<id>/, and `harbor jobs
// resume` then runs the attempt it no longer finds, under a new id, along
// with any others the run had not finished.
func (r *Runner) Retry(id, trial string) error {
	if !r.launched(id) || !validName(trial) {
		return ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, alive, _ := r.stateLocked(id); alive {
		return ErrNotStopped
	}
	dir := filepath.Join(r.JobsDir, id)
	src := filepath.Join(dir, trial)
	if !exists(filepath.Join(dir, "config.json")) || !isTrialDir(src) {
		return ErrNotFound
	}
	// Harbor deletes any folder in the job's that has no result.json, so
	// the old attempt is kept outside it.
	dst := filepath.Join(r.JobsDir, ".bench", "retried", id, trial+"."+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	return r.resumeLocked(id)
}

func (r *Runner) resumeLocked(id string) error {
	dir := filepath.Join(r.JobsDir, id)
	if exists(filepath.Join(dir, "config.json")) {
		if err := prepareResume(dir, id); err != nil {
			return err
		}
	}
	envFile, _ := filepath.Abs(r.EnvFile) // . searches PATH for a bare name
	var cmd *exec.Cmd
	switch {
	case exists(filepath.Join(dir, "config.json")) && r.EnvFile != "" && exists(r.EnvFile):
		// Unlike run, resume has no --env-file; the shell loads it, so
		// the key never passes through bench.
		cmd = exec.Command("/bin/sh", "-c", `set -a && . "$0" && set +a && exec "$@"`,
			envFile, r.Harbor, "jobs", "resume", "-p", dir)
	case exists(filepath.Join(dir, "config.json")):
		cmd = exec.Command(r.Harbor, "jobs", "resume", "-p", dir)
	default:
		args := []string{"run", "-c", r.file(id, ".json"), "-y"}
		if r.EnvFile != "" && exists(r.EnvFile) {
			args = append(args, "--env-file", r.EnvFile)
		}
		cmd = exec.Command(r.Harbor, args...)
	}
	if err := r.spawn(id, cmd); err != nil {
		return err
	}
	os.Remove(r.file(id, ".paused"))
	return nil
}

// prepareResume readies a job folder for `harbor jobs resume`. Harbor leaves
// job_name out of the config.json it writes, and resume then names the job
// after the current time, running every attempt again in a new folder; the
// name is written back first. The attempts of a Harbor that was killed have
// no result.json: resume neither keeps nor removes them, and runs them again
// under new names, so they are removed like the cancelled ones resume removes.
func prepareResume(dir, id string) error {
	cfgFile := filepath.Join(dir, "config.json")
	var cfg map[string]json.RawMessage
	if err := readJSON(cfgFile, &cfg); err != nil {
		return fmt.Errorf("reading %s: %w", cfgFile, err)
	}
	if _, ok := cfg["job_name"]; !ok {
		cfg["job_name"], _ = json.Marshal(id)
		b, err := json.MarshalIndent(cfg, "", "    ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(cfgFile, b, 0o644); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		trial := filepath.Join(dir, e.Name())
		if e.IsDir() && isTrialDir(trial) && !exists(filepath.Join(trial, "result.json")) {
			if err := os.RemoveAll(trial); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) paused(id string) bool {
	return r.launched(id) && exists(r.file(id, ".paused"))
}

// Wait blocks until a run this process started has exited.
func (r *Runner) Wait(id string) {
	if rn := r.get(id); rn != nil {
		<-rn.done
	}
}

func (r *Runner) get(id string) *run {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[id]
}

// file is the path of a run's file in <jobs>/.bench/; with id "", the
// directory itself.
func (r *Runner) file(id, ext string) string {
	return filepath.Join(r.JobsDir, ".bench", id+ext)
}

// ids lists the runs bench started.
func (r *Runner) ids() []string {
	if r == nil {
		return nil
	}
	paths, _ := filepath.Glob(r.file("*", ".json"))
	var ids []string
	for _, p := range paths {
		ids = append(ids, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	return ids
}

func (r *Runner) launched(id string) bool {
	return r != nil && validName(id) && exists(r.file(id, ".json"))
}

// state says whether bench started the job and whether its Harbor is still
// running, with the end of Harbor's output.
func (r *Runner) state(id string) (launched, alive bool, out string) {
	if !r.launched(id) {
		return false, false, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked(id)
}

func (r *Runner) stateLocked(id string) (launched, alive bool, out string) {
	if rn := r.runs[id]; rn != nil {
		select {
		case <-rn.done:
		default:
			alive = true
		}
	} else {
		_, alive = r.pid(id)
	}
	return true, alive, strings.TrimSpace(tailOf(r.file(id, ".log")))
}

// pid returns the pid of the job's Harbor if it is still running. The pid
// counts only while ps shows it running harbor on this job's config, or
// resuming this job's folder, so a
// reused pid or an exited Harbor that is not yet reaped does not.
func (r *Runner) pid(id string) (int, bool) {
	b, err := os.ReadFile(r.file(id, ".pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	cmdline := string(out)
	if err != nil || !strings.Contains(cmdline, r.file(id, ".json")) && !strings.HasSuffix(strings.TrimSpace(cmdline), filepath.Join(r.JobsDir, id)) {
		return 0, false
	}
	return pid, true
}

func (r *Runner) started(id string) *time.Time {
	if st, err := os.Stat(r.file(id, ".json")); err == nil {
		t := st.ModTime()
		return &t
	}
	return nil
}

// planned reads what a run bench started will do from its JobConfig, for
// the time before Harbor writes the job's lock.json.
func (r *Runner) planned(id string) (tasks []string, agent string, models []string, total int) {
	var cfg struct {
		Attempts int           `json:"n_attempts"`
		Agents   []agentConfig `json:"agents"`
		Tasks    []struct {
			Path string `json:"path"`
		} `json:"tasks"`
	}
	if readJSON(r.file(id, ".json"), &cfg) != nil {
		return nil, "", nil, 0
	}
	for _, t := range cfg.Tasks {
		tasks = append(tasks, filepath.Base(t.Path))
	}
	for _, a := range cfg.Agents {
		if agent == "" {
			agent = agentName(a)
		}
		models = appendNew(models, a.ModelName)
	}
	return tasks, agent, models, len(cfg.Tasks) * len(cfg.Agents) * cfg.Attempts
}

// tailSize is how much of Harbor's output a stopped job reports.
const tailSize = 4096

func tailOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > tailSize {
		f.Seek(-tailSize, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

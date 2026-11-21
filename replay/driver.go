package replay

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kavachlabs/kavach/internal/envfacts"
	"github.com/kavachlabs/kavach/journal"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// StepTimeout bounds each step of a replay in a host process (SPEC.md §9.5).
var StepTimeout = 10 * time.Second

const maxRandRequest = 16 << 20

// SplitCommand splits a host command into words like a POSIX shell: words are
// separated by white space, single quotes keep everything literally, double
// quotes keep everything but \", \\, \$ and \`, and a backslash outside quotes
// escapes the next character. Nothing is expanded.
func SplitCommand(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words, cur, inWord = append(words, cur.String()), strings.Builder{}, false
			}
		case c == '\'':
			inWord = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '\''; j++ {
				cur.WriteRune(rs[j])
			}
			if j == len(rs) {
				return nil, errors.New("unterminated ' in host command")
			}
			i = j
		case c == '"':
			inWord = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[j+1]) {
					j++
				}
				cur.WriteRune(rs[j])
			}
			if j == len(rs) {
				return nil, errors.New(`unterminated " in host command`)
			}
			i = j
		case c == '\\':
			if i+1 == len(rs) {
				return nil, errors.New("host command ends in a backslash")
			}
			inWord = true
			i++
			cur.WriteRune(rs[i])
		default:
			inWord = true
			cur.WriteRune(c)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		return nil, errors.New("empty host command")
	}
	return words, nil
}

// RunHost replays j by running the host command (SPEC.md §9.1) and serving
// its reads from the journal: the driver side of the host protocol. It returns
// an error if the replay cannot run, including a host that fails to start, exits
// before it is ready, or breaks the protocol.
func RunHost(j *journal.Journal, command string) (*Result, error) {
	return RunHostWith(j, command, Options{})
}

// RunHostWith is RunHost with options.
func RunHostWith(j *journal.Journal, command string, opts Options) (*Result, error) {
	words, err := SplitCommand(command)
	if err != nil {
		return nil, err
	}
	genesis := extractGenesis(j)
	env := hostEnviron(genesis)
	return runWithHost(j, opts, genesis,
		func(service string, snapshot []byte) (*host, error) {
			return startHostEnv(words, service, snapshot, env)
		},
		func(h *host) { h.close() },
	)
}

// HostPool reuses warm host processes across replay runs that share the same
// normalized genesis environment, falling back to cold starts for legacy hosts
// that do not support the reset message.
type HostPool struct {
	words []string

	mu      sync.Mutex
	idle    map[string][]*host
	closed  bool
	resetOK bool
	noReset bool
}

// NewHostPool creates a pool that runs command to start host processes.
func NewHostPool(command string) (*HostPool, error) {
	words, err := SplitCommand(command)
	if err != nil {
		return nil, err
	}
	return &HostPool{words: words, idle: map[string][]*host{}}, nil
}

// Run replays j using a pooled host process when available.
func (p *HostPool) Run(j *journal.Journal) (*Result, error) {
	return p.RunWith(j, Options{})
}

// RunWith replays j with opts using a pooled host process when available.
func (p *HostPool) RunWith(j *journal.Journal, opts Options) (*Result, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, errors.New("replay: host pool is closed")
	}
	genesis := extractGenesis(j)
	env := hostEnviron(genesis)
	key := envKey(env)
	return runWithHost(j, opts, genesis,
		func(service string, snapshot []byte) (*host, error) {
			return p.acquire(key, service, snapshot, env)
		},
		func(h *host) { p.release(key, h) },
	)
}

// Close ends and closes every idle host process in the pool.
func (p *HostPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var hosts []*host
	for k, list := range p.idle {
		hosts = append(hosts, list...)
		delete(p.idle, k)
	}
	p.mu.Unlock()
	for _, h := range hosts {
		h.close()
	}
	return nil
}

func (p *HostPool) acquire(key, service string, snapshot []byte, env []string) (*host, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("replay: host pool is closed")
	}
	var h *host
	if !p.noReset {
		if list := p.idle[key]; len(list) > 0 {
			h = list[len(list)-1]
			if len(list) == 1 {
				delete(p.idle, key)
			} else {
				p.idle[key] = list[:len(list)-1]
			}
		}
	}
	p.mu.Unlock()

	if h != nil {
		if err := h.reset(service, snapshot); err == nil {
			p.mu.Lock()
			p.resetOK = true
			p.mu.Unlock()
			return h, nil
		}
		h.close()
		var toClose []*host
		p.mu.Lock()
		if !p.resetOK {
			p.noReset = true
			for k, list := range p.idle {
				toClose = append(toClose, list...)
				delete(p.idle, k)
			}
		}
		p.mu.Unlock()
		for _, idleHost := range toClose {
			idleHost.close()
		}
	}

	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, errors.New("replay: host pool is closed")
	}
	return startHostEnv(p.words, service, snapshot, env)
}

func (p *HostPool) release(key string, h *host) {
	if h == nil {
		return
	}
	if h.broken {
		h.close()
		return
	}
	h.stderr.reset()
	p.mu.Lock()
	keep := !p.closed && !p.noReset
	if keep {
		p.idle[key] = append(p.idle[key], h)
	}
	p.mu.Unlock()
	if !keep {
		h.close()
	}
}

func runWithHost(
	j *journal.Journal,
	opts Options,
	genesis []journal.Fact,
	openHost func(service string, snapshot []byte) (*host, error),
	finishHost func(h *host),
) (*Result, error) {
	var h *host
	res, err := replayJournal(j, opts, func(snapshot []byte) (kavach.Handler, error) {
		var err error
		if h, err = openHost(j.Header.Meta.Service, snapshot); err != nil {
			return nil, err
		}
		return h, nil
	})
	if err == nil && h != nil && genesis != nil && h.environment != nil {
		if res.Drift, err = computeDrift(genesis, h.environment); err != nil {
			h.broken = true
			err = fmt.Errorf("host sent an invalid environment: %w", err)
		}
	}
	if h != nil {
		finishHost(h)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func extractGenesis(j *journal.Journal) []journal.Fact {
	recs := j.Records
	if j.Header.Meta.Start == journal.StartSnapshot && len(recs) > 0 {
		recs = recs[1:]
	}
	if g := genesisEnv(recs); g != nil {
		return g.Facts
	}
	return nil
}

func envKey(env []string) string {
	sorted := append([]string(nil), env...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\x00")
}

// hostEnviron is the environment a host is started with (SPEC.md §9.1).
func hostEnviron(genesis []journal.Fact) []string {
	env := []string{}
	named := map[string]bool{}
	for _, f := range genesis {
		name, ok := strings.CutPrefix(f.Key, "env.")
		if !ok || name == "" || strings.Contains(name, "=") {
			continue
		}
		named[name] = true
		switch f.Form {
		case journal.FactValue:
			env = append(env, name+"="+string(f.Value))
		case journal.FactSHA256:
			if v, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+v)
			}
		}
	}
	for _, name := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(name); ok && !named[name] {
			env = append(env, name+"="+v)
		}
	}
	return env
}

type host struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	lines       chan []byte
	quit        chan struct{}
	stderr      *tail
	invariants  []string
	environment map[string]envfacts.JSONFact
	broken      bool

	failedInvariant, failedDetail string
	waitOnce                      sync.Once
	waitErr                       error
	closeOnce                     sync.Once
}

type wire struct {
	T           string                       `json:"t"`
	Protocol    int                          `json:"protocol"`
	SDK         string                       `json:"sdk"`
	Invariants  []string                     `json:"invariants"`
	Environment map[string]envfacts.JSONFact `json:"environment"`
	Gateway     string                       `json:"gateway"`
	Request     []byte                       `json:"request"`
	Key         string                       `json:"key"`
	N           int                          `json:"n"`
	Sink        string                       `json:"sink"`
	Data        []byte                       `json:"data"`
	Outcome     string                       `json:"outcome"`
	Message     string                       `json:"message"`
	Detail      string                       `json:"detail"`
}

var (
	errHostGone = errors.New("host closed its output")
	errTimeout  = errors.New("timed out")
)

func startHost(words []string, service string, snapshot []byte, genesis []journal.Fact) (*host, error) {
	return startHostEnv(words, service, snapshot, hostEnviron(genesis))
}

func startHostEnv(words []string, service string, snapshot []byte, env []string) (*host, error) {
	cmd := exec.Command(words[0], append(append([]string{}, words[1:]...), kavach.HostCommand)...)
	cmd.Env = env
	h := &host{cmd: cmd, quit: make(chan struct{}), lines: make(chan []byte), stderr: &tail{}}
	cmd.Stderr = h.stderr
	cmd.WaitDelay = time.Second
	var err error
	if h.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting host: %w", err)
	}
	go func() {
		defer close(h.lines)
		r := bufio.NewReader(stdout)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			select {
			case h.lines <- line:
			case <-h.quit:
				return
			}
		}
	}()

	fail := func(err error) (*host, error) {
		h.close()
		return nil, err
	}
	if err := h.send(initMsg("hello", service, snapshot)); err != nil {
		return fail(h.beforeReady(err))
	}
	m, err := h.recv(time.Now().Add(StepTimeout))
	if err != nil {
		return fail(h.beforeReady(err))
	}
	if err := h.expectReady(m); err != nil {
		return fail(err)
	}
	h.invariants, h.environment = m.Invariants, m.Environment
	return h, nil
}

func initMsg(t, service string, snapshot []byte) map[string]any {
	msg := map[string]any{"t": t, "protocol": 1, "service": service, "start": journal.StartGenesis, "mode": "process"}
	if snapshot != nil {
		msg["start"], msg["snapshot"] = journal.StartSnapshot, b64(snapshot)
	}
	return msg
}

func (h *host) expectReady(m wire) error {
	switch {
	case m.T == "fatal":
		return fmt.Errorf("host cannot continue: %s", m.Message)
	case m.T != "ready":
		return fmt.Errorf("host sent %q, want ready", m.T)
	case m.Protocol != 1:
		return fmt.Errorf("host speaks protocol %d, want 1", m.Protocol)
	}
	return nil
}

func (h *host) reset(service string, snapshot []byte) error {
	if err := h.send(initMsg("reset", service, snapshot)); err != nil {
		h.broken = true
		return err
	}
	m, err := h.recv(time.Now().Add(StepTimeout))
	if err != nil {
		h.broken = true
		if errors.Is(err, errTimeout) {
			if h.cmd != nil && h.cmd.Process != nil {
				h.cmd.Process.Kill()
			}
			return fmt.Errorf("host did not send ready within %s", StepTimeout)
		}
		return err
	}
	if err := h.expectReady(m); err != nil {
		h.broken = true
		return err
	}
	h.invariants = m.Invariants
	if m.Environment != nil {
		h.environment = m.Environment
	}
	h.failedInvariant, h.failedDetail = "", ""
	h.stderr.reset()
	return nil
}

func (h *host) beforeReady(err error) error {
	if errors.Is(err, errTimeout) {
		return fmt.Errorf("host did not send ready within %s", StepTimeout)
	}
	if errors.Is(err, errHostGone) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) {
		return fmt.Errorf("host exited before ready (%s)%s; does the command act as a host when run with %s?", h.exitStatus(), h.stderr.suffix(), kavach.HostCommand)
	}
	return err
}

func (h *host) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = h.stdin.Write(append(b, '\n'))
	return err
}

func (h *host) recv(deadline time.Time) (wire, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case line, ok := <-h.lines:
		if !ok {
			return wire{}, errHostGone
		}
		var m wire
		if err := json.Unmarshal(line, &m); err != nil || m.T == "" {
			return wire{}, fmt.Errorf("host wrote a line that is not a protocol message: %.200q", line)
		}
		return m, nil
	case <-timer.C:
		return wire{}, errTimeout
	}
}

func (h *host) exit() error {
	h.waitOnce.Do(func() {
		done := make(chan error, 1)
		go func() { done <- h.cmd.Wait() }()
		select {
		case h.waitErr = <-done:
		case <-time.After(2 * time.Second):
			h.cmd.Process.Kill()
			h.waitErr = <-done
		}
	})
	return h.waitErr
}

func (h *host) exitStatus() string {
	if err := h.exit(); err != nil {
		return err.Error()
	}
	return "exit status 0"
}

func (h *host) close() {
	h.broken = true
	h.closeOnce.Do(func() {
		h.send(map[string]any{"t": "end"})
		h.stdin.Close()
		h.exit()
		close(h.quit)
	})
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// Handle sends the input to the host as a step and serves its requests from
// env, which must be the replay's. Nondeterminism ends the step with an abort
// (SPEC.md §9.4) and is re-raised.
func (h *host) Handle(env kavach.Env, in kavach.Input) error {
	h.failedInvariant = ""
	renv := env.(*replayEnv)
	deadline := time.Now().Add(StepTimeout)
	step := map[string]any{"t": "step", "seq": strconv.FormatUint(renv.input, 10), "source": in.Source, "position": in.Position, "data": b64(in.Data)}
	if err := h.send(step); err != nil {
		h.broken = true
		panic(hostCrash(h.crashDetail()))
	}
	for {
		m, err := h.recv(deadline)
		if err != nil {
			return h.stepFailure(err)
		}
		reply, nd := h.serve(renv, m)
		if nd != nil {
			h.abort(nd.msg, deadline)
			panic(*nd)
		}
		switch m.T {
		case "done":
			return h.finish(m)
		case "emit", "clock", "rand", "gateway", "config":
		default:
			h.broken = true
			panic(hostFailure{fmt.Errorf("host sent %q during a step", m.T)})
		}
		if reply != nil {
			if err := h.send(reply); err != nil {
				h.broken = true
				panic(hostCrash(h.crashDetail()))
			}
		}
	}
}

// serve answers one request from the host. A nondeterminism panic of env is
// returned instead of propagating.
func (h *host) serve(env *replayEnv, m wire) (reply map[string]any, nd *nondeterminism) {
	defer func() {
		if v := recover(); v != nil {
			n, ok := v.(nondeterminism)
			if !ok {
				panic(v)
			}
			nd = &n
		}
	}()
	switch m.T {
	case "clock":
		return map[string]any{"t": "clock", "unix_nanos": strconv.FormatInt(env.Now().UnixNano(), 10)}, nil
	case "rand":
		if m.N < 1 || m.N > maxRandRequest {
			h.broken = true
			panic(hostFailure{fmt.Errorf("host asked for %d random bytes", m.N)})
		}
		p := make([]byte, m.N)
		env.Read(p)
		return map[string]any{"t": "rand", "data": b64(p)}, nil
	case "gateway":
		resp, err := env.Query(m.Gateway, m.Request)
		if err != nil {
			return map[string]any{"t": "gateway", "error": err.Error()}, nil
		}
		return map[string]any{"t": "gateway", "response": b64(resp)}, nil
	case "config":
		v, ok := env.Config(m.Key)
		if !ok {
			return map[string]any{"t": "config", "present": false}, nil
		}
		return map[string]any{"t": "config", "present": true, "value": b64(v)}, nil
	case "emit":
		env.Emit(m.Sink, m.Data)
	}
	return nil, nil
}

// abort tells the host to stop the step and waits for its done, answering any
// request that was already on its way with abort. The status is
// nondeterministic whatever the host does, so a host that fails is not reported.
func (h *host) abort(detail string, deadline time.Time) {
	msg := map[string]any{"t": "abort", "detail": detail}
	if h.send(msg) != nil {
		h.broken = true
		return
	}
	for {
		m, err := h.recv(deadline)
		if err != nil {
			h.broken = true
			if errors.Is(err, errTimeout) && h.cmd != nil && h.cmd.Process != nil {
				h.cmd.Process.Kill()
			}
			return
		}
		if m.T == "fatal" {
			h.broken = true
			return
		}
		if m.T == "done" {
			return
		}
		switch m.T {
		case "clock", "rand", "gateway", "config":
			if h.send(msg) != nil {
				h.broken = true
				return
			}
		}
	}
}

func (h *host) finish(m wire) error {
	switch m.Outcome {
	case "ok":
		return nil
	case "panic":
		panic(m.Message)
	case "error":
		return errors.New(m.Message)
	case "invariant":
		h.failedInvariant, h.failedDetail = m.Message, m.Detail
		return nil
	}
	h.broken = true
	panic(hostFailure{fmt.Errorf("host ended a step with outcome %q", m.Outcome)})
}

func (h *host) stepFailure(err error) error {
	h.broken = true
	switch {
	case errors.Is(err, errTimeout):
		h.cmd.Process.Kill()
		panic(hostTimeout(fmt.Sprintf("host did not finish the step within %s", StepTimeout)))
	case errors.Is(err, errHostGone):
		panic(hostCrash(h.crashDetail()))
	}
	panic(hostFailure{err})
}

func (h *host) crashDetail() string {
	return fmt.Sprintf("host exited during the step (%s)%s", h.exitStatus(), h.stderr.suffix())
}

// Invariants reports the invariant that failed after the last step, if any,
// among those the host declared.
func (h *host) Invariants() []kavach.Invariant {
	var out []kavach.Invariant
	for _, name := range h.invariants {
		out = append(out, kavach.Invariant{Name: name, Check: func() error {
			if name != h.failedInvariant {
				return nil
			}
			if h.failedDetail == "" {
				return errors.New("failed")
			}
			return errors.New(h.failedDetail)
		}})
	}
	return out
}

type tail struct {
	mu sync.Mutex
	b  []byte
}

const tailSize = 1024

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailSize {
		t.b = t.b[len(t.b)-tailSize:]
	}
	return len(p), nil
}

func (t *tail) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := string(bytes.TrimSpace(t.b)); s != "" {
		return ": " + s
	}
	return ""
}

func (t *tail) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = t.b[:0]
}

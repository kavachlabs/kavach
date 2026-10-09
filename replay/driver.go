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
	words, err := SplitCommand(command)
	if err != nil {
		return nil, err
	}
	var genesis []journal.Fact
	recs := j.Records
	if j.Header.Meta.Start == journal.StartSnapshot && len(recs) > 0 {
		recs = recs[1:]
	}
	if g := genesisEnv(recs); g != nil {
		genesis = g.Facts
	}
	var h *host
	res, err := replayJournal(j, func(snapshot []byte) (kavach.Handler, error) {
		var err error
		if h, err = startHost(words, j.Header.Meta.Service, snapshot, genesis); err != nil {
			return nil, err
		}
		return h, nil
	})
	if h != nil {
		h.close()
	}
	if err != nil {
		return nil, err
	}
	if genesis != nil && h.environment != nil {
		if res.Drift, err = computeDrift(genesis, h.environment); err != nil {
			return nil, fmt.Errorf("host sent an invalid environment: %w", err)
		}
	}
	return res, nil
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

// host is a running host process. As a Handler it proxies each step to the
// process, serving the reads it makes from the replay's Env.
type host struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	lines       chan []byte
	quit        chan struct{}
	stderr      *tail
	invariants  []string
	environment map[string]envfacts.JSONFact

	failedInvariant, failedDetail string
	waitOnce                      sync.Once
	waitErr                       error
}

// wire is a message from the host.
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
	cmd := exec.Command(words[0], append(append([]string{}, words[1:]...), kavach.HostCommand)...)
	cmd.Env = hostEnviron(genesis)
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

	hello := map[string]any{"t": "hello", "protocol": 1, "service": service, "start": journal.StartGenesis, "mode": "process"}
	if snapshot != nil {
		hello["start"], hello["snapshot"] = journal.StartSnapshot, base64.StdEncoding.EncodeToString(snapshot)
	}
	fail := func(err error) (*host, error) {
		h.close()
		return nil, err
	}
	if err := h.send(hello); err != nil {
		return fail(h.beforeReady(err))
	}
	m, err := h.recv(time.Now().Add(StepTimeout))
	if err != nil {
		return fail(h.beforeReady(err))
	}
	switch {
	case m.T == "fatal":
		return fail(fmt.Errorf("host cannot continue: %s", m.Message))
	case m.T != "ready":
		return fail(fmt.Errorf("host sent %q, want ready", m.T))
	case m.Protocol != 1:
		return fail(fmt.Errorf("host speaks protocol %d, want 1", m.Protocol))
	}
	h.invariants, h.environment = m.Invariants, m.Environment
	return h, nil
}

// beforeReady describes why a host failed before sending ready.
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

// recv reads the host's next message. It returns errHostGone when the host has
// closed its output and errTimeout at deadline. A line that is not a message
// is a protocol violation.
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

// close ends the session: the host is asked to exit and is killed if it
// does not.
func (h *host) close() {
	h.send(map[string]any{"t": "end"})
	h.stdin.Close()
	h.exit()
	close(h.quit)
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
			panic(hostFailure{fmt.Errorf("host sent %q during a step", m.T)})
		}
		if reply != nil {
			if err := h.send(reply); err != nil {
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
		return
	}
	for {
		m, err := h.recv(deadline)
		if err != nil || m.T == "done" || m.T == "fatal" {
			return
		}
		switch m.T {
		case "clock", "rand", "gateway", "config":
			if h.send(msg) != nil {
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
	panic(hostFailure{fmt.Errorf("host ended a step with outcome %q", m.Outcome)})
}

// stepFailure ends a step whose host stopped answering.
func (h *host) stepFailure(err error) error {
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

// tail keeps the end of a host's standard error for failure messages.
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

// suffix is the tail of standard error as the end of a message, or "".
func (t *tail) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := string(bytes.TrimSpace(t.b)); s != "" {
		return ": " + s
	}
	return ""
}

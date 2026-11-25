// Package host implements the deterministic kavach-host stdio JSON protocol
// loop (SPEC §9), 100% wire-compatible with github.com/kavachlabs/kavach/replay.
package host

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/kavachlabs/kavach-go/invariants"
	"github.com/kavachlabs/kavach-go/matcher"
)

// HostCommand is the final CLI argument that puts a service binary in host mode (SPEC §9.1).
const HostCommand = "kavach-host"

// ProtocolVersion is the SPEC §9 host protocol version.
const ProtocolVersion = 1

// driverMsg matches every inbound JSON message sent by replay/driver.go (SPEC §9.2).
type driverMsg struct {
	T         string          `json:"t"`
	Protocol  int             `json:"protocol"`
	Service   string          `json:"service"`
	Start     string          `json:"start"`
	Mode      string          `json:"mode"`
	Snapshot  []byte          `json:"snapshot"`
	Seq       json.RawMessage `json:"seq"`
	Source    string          `json:"source"`
	Position  string          `json:"position"`
	Data      []byte          `json:"data"`
	UnixNanos string          `json:"unix_nanos"`
	Response  []byte          `json:"response"`
	Error     *string         `json:"error"`
	Present   bool            `json:"present"`
	Value     []byte          `json:"value"`
	Detail    string          `json:"detail"`
}

func parseSeq(raw json.RawMessage) uint64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, _ := strconv.ParseUint(s, 10, 64)
		return n
	}
	var n uint64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	return 0
}

// StepInput is one step delivered by the replay driver.
type StepInput struct {
	Seq      uint64
	Source   string
	Position string
	Data     []byte
}

// StepOutcome summarizes the result of executing one step in RunStep.
type StepOutcome struct {
	Err             error
	InvariantName   string
	InvariantDetail string
}

// Options configure a host protocol session served by Serve.
type Options struct {
	// SDKVersion is reported in the "ready" message (e.g. "kavach-go/0.2.0").
	SDKVersion string
	// InvariantNames returns the names of all registered code + YAML invariants.
	InvariantNames func() []string
	// OnInit is called on "hello" and "reset" to initialize/reset handler state.
	OnInit func(service, start string, snapshot []byte) error
	// RunStep executes one step with the provided host Env and returns its outcome.
	RunStep func(in StepInput, env *Env) StepOutcome
}

type abortStep struct {
	detail string
}

type hostFatal struct {
	err error
}

type session struct {
	mu  sync.Mutex
	in  *bufio.Reader
	out io.Writer
}

func (s *session) sendLocked(msg map[string]any) {
	b, err := json.Marshal(msg)
	if err == nil {
		_, err = s.out.Write(append(b, '\n'))
	}
	if err != nil {
		panic(hostFatal{err})
	}
}

func (s *session) recvLocked() driverMsg {
	line, err := s.in.ReadBytes('\n')
	if err != nil {
		panic(hostFatal{errors.New("driver closed the session without end")})
	}
	var m driverMsg
	if err := json.Unmarshal(line, &m); err != nil {
		panic(hostFatal{fmt.Errorf("malformed message from the driver: %w", err)})
	}
	return m
}

func (s *session) send(msg map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendLocked(msg)
}

func (s *session) recv() driverMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvLocked()
}

// Env is the per-step host environment communicating synchronously with the
// replay driver over stdio JSON. All methods are safe for concurrent goroutines
// spawned inside a single step (e.g. errgroup.Group).
type Env struct {
	s       *session
	mu      sync.Mutex
	aborted bool
	closed  bool
	outputs []invariants.Output
}

func (e *Env) close() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
}

// Aborted reports whether the driver aborted this step.
func (e *Env) Aborted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.aborted
}

// Outputs returns a snapshot of all outputs emitted during this step.
func (e *Env) Outputs() []invariants.Output {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]invariants.Output, len(e.outputs))
	copy(out, e.outputs)
	return out
}

func (e *Env) ask(msg map[string]any) (driverMsg, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return driverMsg{}, false
	}
	if e.aborted {
		panic(abortStep{})
	}
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	e.s.sendLocked(msg)
	m := e.s.recvLocked()
	switch m.T {
	case "abort":
		e.aborted = true
		panic(abortStep{detail: m.Detail})
	case msg["t"]:
		return m, true
	}
	panic(hostFatal{fmt.Errorf("want %v, got %q", msg["t"], m.T)})
}

// Now implements clock.Provider by requesting the recorded timestamp from the driver.
func (e *Env) Now() time.Time {
	m, ok := e.ask(map[string]any{"t": "clock"})
	if !ok {
		return time.Unix(0, 0).UTC()
	}
	n, err := strconv.ParseInt(m.UnixNanos, 10, 64)
	if err != nil {
		panic(hostFatal{fmt.Errorf("bad unix_nanos %q", m.UnixNanos)})
	}
	return time.Unix(0, n).UTC()
}

// ReadRand implements clock.Provider by requesting len(p) recorded random bytes.
func (e *Env) ReadRand(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	m, ok := e.ask(map[string]any{"t": "rand", "n": len(p)})
	if !ok {
		return 0, errors.New("kavach: step already ended")
	}
	if len(m.Data) != len(p) {
		panic(hostFatal{fmt.Errorf("asked for %d random bytes, got %d", len(p), len(m.Data))})
	}
	return copy(p, m.Data), nil
}

// Read implements io.Reader as an alias for ReadRand.
func (e *Env) Read(p []byte) (int, error) {
	return e.ReadRand(p)
}

// Query sends a gateway request to the replay driver and returns the recorded
// response or injected/recorded error.
func (e *Env) Query(gateway string, request []byte) ([]byte, error) {
	m, ok := e.ask(map[string]any{
		"t":       "gateway",
		"gateway": gateway,
		"request": b64(request),
		"scope":   "remote",
	})
	if !ok {
		return nil, errors.New("kavach: step already ended")
	}
	if m.Error != nil {
		msg := *m.Error
		if code := matcher.DetectFaultCode(msg); code != "" {
			return nil, &matcher.FaultError{Code: code, Message: msg, Gateway: gateway}
		}
		return nil, errors.New(msg)
	}
	return m.Response, nil
}

// Config queries a recorded configuration key from the replay driver.
func (e *Env) Config(key string) ([]byte, bool) {
	m, ok := e.ask(map[string]any{"t": "config", "key": key})
	if !ok {
		return nil, false
	}
	return m.Value, m.Present
}

// Emit sends an output emit message to the replay driver and records it on Env.
func (e *Env) Emit(sink string, data []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if e.aborted {
		panic(abortStep{})
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	e.outputs = append(e.outputs, invariants.Output{Sink: sink, Data: cp})
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	e.s.sendLocked(map[string]any{
		"t":     "emit",
		"sink":  sink,
		"data":  b64(data),
		"scope": "remote",
	})
}

// Serve runs the deterministic kavach-host protocol loop over in and out until
// the driver sends "end" or a fatal protocol error occurs.
func Serve(opts Options, in io.Reader, out io.Writer) (err error) {
	if opts.RunStep == nil {
		return errors.New("host: RunStep is required")
	}
	sdk := opts.SDKVersion
	if sdk == "" {
		sdk = "kavach-go/0.2.0"
	}
	s := &session{in: bufio.NewReader(in), out: out}
	defer func() {
		if v := recover(); v != nil {
			f, ok := v.(hostFatal)
			if !ok {
				panic(v)
			}
			err = f.err
			func() {
				defer func() { _ = recover() }()
				s.send(map[string]any{"t": "fatal", "message": f.err.Error()})
			}()
		}
	}()

	getNames := func() []string {
		if opts.InvariantNames != nil {
			if names := opts.InvariantNames(); names != nil {
				return names
			}
		}
		return []string{}
	}

	hello := s.recv()
	if hello.T != "hello" || hello.Protocol != ProtocolVersion {
		panic(hostFatal{fmt.Errorf("want hello for protocol %d, got %q protocol %d", ProtocolVersion, hello.T, hello.Protocol)})
	}
	if opts.OnInit != nil {
		if initErr := opts.OnInit(hello.Service, hello.Start, hello.Snapshot); initErr != nil {
			panic(hostFatal{initErr})
		}
	}
	s.send(map[string]any{
		"t":          "ready",
		"protocol":   ProtocolVersion,
		"sdk":        sdk,
		"invariants": getNames(),
		"environment": map[string]any{
			"host.runtime": map[string]any{
				"value": b64([]byte(runtime.Version())),
			},
		},
	})

	for {
		m := s.recv()
		switch m.T {
		case "end":
			return nil
		case "reset":
			if m.Protocol != 0 && m.Protocol != ProtocolVersion {
				panic(hostFatal{fmt.Errorf("want reset for protocol %d, got protocol %d", ProtocolVersion, m.Protocol)})
			}
			if opts.OnInit != nil {
				if initErr := opts.OnInit(m.Service, m.Start, m.Snapshot); initErr != nil {
					panic(hostFatal{initErr})
				}
			}
			s.send(map[string]any{
				"t":          "ready",
				"protocol":   ProtocolVersion,
				"sdk":        sdk,
				"invariants": getNames(),
			})
		case "step":
			s.runStep(opts, m)
		default:
			panic(hostFatal{fmt.Errorf("unexpected %q between steps", m.T)})
		}
	}
}

func (s *session) runStep(opts Options, m driverMsg) {
	env := &Env{s: s}
	stepIn := StepInput{
		Seq:      parseSeq(m.Seq),
		Source:   m.Source,
		Position: m.Position,
		Data:     m.Data,
	}

	pv, stack, outcome := func() (pv any, stack []byte, out StepOutcome) {
		defer func() {
			if v := recover(); v != nil {
				pv, stack = v, debug.Stack()
			}
		}()
		return nil, nil, opts.RunStep(stepIn, env)
	}()

	env.close()

	done := map[string]any{"t": "done", "outcome": "ok"}
	fail := func(kind, message, detail string) {
		done["outcome"] = kind
		done["message"] = message
		if detail != "" {
			done["detail"] = detail
		}
	}

	switch {
	case pv != nil:
		switch v := pv.(type) {
		case abortStep:
			done["outcome"] = "aborted"
		case hostFatal:
			panic(v)
		default:
			fail("panic", fmt.Sprint(pv), string(stack))
		}
	case outcome.InvariantName != "":
		fail("invariant", outcome.InvariantName, outcome.InvariantDetail)
	case outcome.Err != nil:
		fail("error", outcome.Err.Error(), "")
	}
	s.send(done)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

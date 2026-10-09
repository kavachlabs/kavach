package kavach

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/kavachlabs/kavach/internal/envfacts"
	"github.com/kavachlabs/kavach/journal"
)

// driverMsg is a message from the driver (SPEC.md §9.2).
type driverMsg struct {
	T         string  `json:"t"`
	Protocol  int     `json:"protocol"`
	Start     string  `json:"start"`
	Snapshot  []byte  `json:"snapshot"`
	Source    string  `json:"source"`
	Position  string  `json:"position"`
	Data      []byte  `json:"data"`
	UnixNanos string  `json:"unix_nanos"`
	Response  []byte  `json:"response"`
	Error     *string `json:"error"`
	Present   bool    `json:"present"`
	Value     []byte  `json:"value"`
	Detail    string  `json:"detail"`
}

// abortStep and hostFatal are panic values of the host's Env: the driver
// stopped the step, or the session cannot go on.
type abortStep struct{}

type hostFatal struct{ err error }

type session struct {
	in  *bufio.Reader
	out io.Writer
}

func (s *session) send(msg map[string]any) {
	b, err := json.Marshal(msg)
	if err == nil {
		_, err = s.out.Write(append(b, '\n'))
	}
	if err != nil {
		panic(hostFatal{err})
	}
}

func (s *session) recv() driverMsg {
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

// ServeHost is the host side of the protocol (SPEC.md §9): it runs fresh
// handlers from newHandler one step at a time for a driver on in and out. It
// returns nil when the driver ends the session.
func ServeHost(newHandler func() Handler, in io.Reader, out io.Writer) (err error) {
	s := &session{in: bufio.NewReader(in), out: out}
	defer func() {
		if v := recover(); v != nil {
			f, ok := v.(hostFatal)
			if !ok {
				panic(v)
			}
			err = f.err
			func() {
				defer func() { recover() }()
				s.send(map[string]any{"t": "fatal", "message": f.err.Error()})
			}()
		}
	}()

	hello := s.recv()
	if hello.T != "hello" || hello.Protocol != 1 {
		panic(hostFatal{fmt.Errorf("want hello for protocol 1, got %q protocol %d", hello.T, hello.Protocol)})
	}
	h := newHandler()
	if hello.Start == journal.StartSnapshot {
		sn, ok := h.(Snapshotter)
		if !ok {
			panic(hostFatal{errors.New("the handler does not implement Snapshotter")})
		}
		if err := sn.Restore(hello.Snapshot); err != nil {
			panic(hostFatal{fmt.Errorf("restoring snapshot: %w", err)})
		}
	}
	names := []string{}
	if c, ok := h.(Checker); ok {
		for _, inv := range c.Invariants() {
			names = append(names, inv.Name)
		}
	}
	facts := append(envfacts.New().Facts(nil), journal.Fact{Key: "host.runtime", Form: journal.FactValue, Value: []byte(runtime.Version())})
	s.send(map[string]any{"t": "ready", "protocol": 1, "sdk": "kavach-go/" + Version, "invariants": names, "environment": envfacts.ToJSON(facts)})

	for {
		switch m := s.recv(); m.T {
		case "end":
			return nil
		case "step":
			s.step(h, m)
		default:
			panic(hostFatal{fmt.Errorf("unexpected %q between steps", m.T)})
		}
	}
}

func (s *session) step(h Handler, m driverMsg) {
	env := &hostEnv{s: s}
	pv, stack, herr := func() (pv any, stack []byte, err error) {
		defer func() {
			if v := recover(); v != nil {
				pv, stack = v, debug.Stack()
			}
		}()
		return nil, nil, h.Handle(env, Input{Source: m.Source, Position: m.Position, Data: m.Data})
	}()
	done := map[string]any{"t": "done", "outcome": "ok"}
	fail := func(outcome, message string, detail []byte) {
		done["outcome"], done["message"] = outcome, message
		if len(detail) > 0 {
			done["detail"] = string(detail)
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
			fail("panic", fmt.Sprint(pv), stack)
		}
	case herr != nil:
		fail("error", herr.Error(), nil)
	default:
		if name, ierr := checkInvariants(h); ierr != nil {
			fail("invariant", name, []byte(ierr.Error()))
		}
	}
	s.send(done)
}

// hostEnv is the Env of a handler running under a driver: every read is a
// request the driver answers.
type hostEnv struct {
	s       *session
	aborted bool
}

func (e *hostEnv) ask(msg map[string]any) driverMsg {
	if e.aborted {
		panic(abortStep{})
	}
	e.s.send(msg)
	m := e.s.recv()
	switch m.T {
	case "abort":
		e.aborted = true
		panic(abortStep{})
	case msg["t"]:
		return m
	}
	panic(hostFatal{fmt.Errorf("want %v, got %q", msg["t"], m.T)})
}

func (e *hostEnv) Now() time.Time {
	m := e.ask(map[string]any{"t": "clock"})
	n, err := strconv.ParseInt(m.UnixNanos, 10, 64)
	if err != nil {
		panic(hostFatal{fmt.Errorf("bad unix_nanos %q", m.UnixNanos)})
	}
	return time.Unix(0, n).UTC()
}

func (e *hostEnv) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	m := e.ask(map[string]any{"t": "rand", "n": len(p)})
	if len(m.Data) != len(p) {
		panic(hostFatal{fmt.Errorf("asked for %d random bytes, got %d", len(p), len(m.Data))})
	}
	return copy(p, m.Data), nil
}

func (e *hostEnv) Query(gateway string, request []byte) ([]byte, error) {
	m := e.ask(map[string]any{"t": "gateway", "gateway": gateway, "request": b64(request), "scope": "remote"})
	if m.Error != nil {
		return nil, errors.New(*m.Error)
	}
	return m.Response, nil
}

func (e *hostEnv) Config(key string) ([]byte, bool) {
	m := e.ask(map[string]any{"t": "config", "key": key})
	return m.Value, m.Present
}

func (e *hostEnv) Emit(sink string, data []byte) {
	if e.aborted {
		panic(abortStep{})
	}
	e.s.send(map[string]any{"t": "emit", "sink": sink, "data": b64(data), "scope": "remote"})
}

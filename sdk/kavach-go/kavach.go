package kavach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kavachlabs/kavach-go/clock"
	"github.com/kavachlabs/kavach-go/host"
	"github.com/kavachlabs/kavach-go/invariants"
	"github.com/kavachlabs/kavach-go/matcher"
	"github.com/kavachlabs/kavach-go/ring"
)

// StepRunner executes a single deterministic step given ctx and Input.
// Inbound boundary adapters (kavachhttp, kavachkgo consumer) implement StepRunner
// so kv.ServeOrReplay drives them seamlessly in Replay mode.
type StepRunner interface {
	RunStep(ctx context.Context, in Input) error
}

// StepFunc adapts a plain function to StepRunner.
type StepFunc func(ctx context.Context, in Input) error

// RunStep implements StepRunner.
func (f StepFunc) RunStep(ctx context.Context, in Input) error { return f(ctx, in) }

// Server is implemented by long-running service runners (such as Kafka consumers
// or wrapped servers) that run normally outside Replay mode.
type Server interface {
	Serve(ctx context.Context) error
}

// Starter is implemented by consumer groups whose run loop has signature Start(ctx)
// (such as Lago's kafka.ConsumerGroup).
type Starter interface {
	Start(ctx context.Context)
}

// Kavach is the root runtime handle returned by Init.
// It is safe for concurrent use across all wrapped drivers and handlers.
type Kavach struct {
	cfg     Config
	mode    Mode
	writer  *ring.Writer
	initErr error
	closed  atomic.Bool
	stepSeq atomic.Uint64

	activeStep atomic.Pointer[StepContext]

	mu         sync.Mutex
	eval       *invariants.Evaluator
	codeInvs   []codeInvariant
	attempts   map[string]int
	faultRules []*matcher.FaultRule
}

// Init initializes a Kavach runtime according to cfg.
//
// In production, Init never panics or blocks service startup: if RingPath cannot
// be opened, Init records the error (inspectable via InitError) and drops writes
// cleanly without impacting production latency or availability.
func Init(cfg Config) *Kavach {
	resolved := resolveConfig(cfg)
	kv := &Kavach{
		cfg:      resolved,
		mode:     resolved.Mode,
		eval:     invariants.NewEvaluator(nil),
		attempts: make(map[string]int),
	}

	if resolved.InvariantsPath != "" {
		if err := kv.LoadInvariants(resolved.InvariantsPath); err != nil {
			kv.initErr = err
		}
	}

	if kv.mode == ModeRecord && resolved.RingPath != "" {
		capBytes := uint64(resolved.RingCapacity)
		if capBytes == 0 {
			capBytes = ring.DefaultCapacity
		}
		meta := ring.OpenMeta{
			Protocol: ring.ProtocolVersion,
			Service:  resolved.Service,
			Start:    "genesis",
			Producer: "kavach-go/" + Version,
		}
		w, err := ring.OpenPath(resolved.RingPath, capBytes, meta)
		if err != nil {
			// Production write-path safety guarantee: degrade gracefully, never crash app.
			if kv.initErr == nil {
				kv.initErr = err
			}
		} else {
			kv.writer = w
		}
	}

	return kv
}

// Mode returns the resolved active Mode (ModeOff, ModeRecord, or ModeReplay).
func (kv *Kavach) Mode() Mode {
	if kv == nil {
		return ModeOff
	}
	return kv.mode
}

// IsOff reports whether Kavach is inactive.
func (kv *Kavach) IsOff() bool { return kv.Mode() == ModeOff }

// IsRecord reports whether Kavach is recording traffic.
func (kv *Kavach) IsRecord() bool { return kv.Mode() == ModeRecord }

// IsReplay reports whether Kavach is running in deterministic Replay mode.
func (kv *Kavach) IsReplay() bool { return kv.Mode() == ModeReplay }

// Service returns the configured service name.
func (kv *Kavach) Service() string {
	if kv == nil {
		return ""
	}
	return kv.cfg.Service
}

// InitError returns any non-fatal initialization error (e.g. unwritable RingPath
// or malformed invariants.yaml).
func (kv *Kavach) InitError() error {
	if kv == nil {
		return nil
	}
	return kv.initErr
}

// RingWriter returns the underlying KVRING02 / stream Writer in ModeRecord, or nil.
func (kv *Kavach) RingWriter() *ring.Writer {
	if kv == nil {
		return nil
	}
	return kv.writer
}

// Close flushes and closes the underlying ring/stream writer (emitting FrameClose).
func (kv *Kavach) Close() error {
	if kv == nil || !kv.closed.CompareAndSwap(false, true) {
		return nil
	}
	if kv.writer != nil {
		return kv.writer.Close()
	}
	return nil
}

// Scrub deterministically scrubs PII from data using this instance's configured
// PII ScrubMode and HMAC-SHA256 key.
func (kv *Kavach) Scrub(data []byte) []byte {
	if kv == nil {
		return data
	}
	return ScrubBytes(kv.cfg.PII, kv.cfg.PIIKey, data)
}

// ScrubHeaders deterministically scrubs PII from a headers map.
func (kv *Kavach) ScrubHeaders(headers map[string]string) map[string]string {
	if kv == nil {
		return headers
	}
	return ScrubHeaders(kv.cfg.PII, kv.cfg.PIIKey, headers)
}

// InjectFault registers a driver-level fault rule that applies across
// subsequent ReplayGateway calls (useful for in-process fault-injection tests).
// Rules with Once: true fire once across the session; Nth counts matching calls
// across steps until Reset.
func (kv *Kavach) InjectFault(rule FaultRule) {
	if kv == nil {
		return
	}
	cp := rule
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.faultRules = append(kv.faultRules, &cp)
}

func (kv *Kavach) matchFault(call GatewayCall) (*matcher.FaultError, bool) {
	if kv == nil {
		return nil, false
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	for _, rule := range kv.faultRules {
		if rule.Match(call) {
			msg := matcher.ExpandFault(rule.ErrString)
			code := matcher.DetectFaultCode(rule.ErrString)
			if code == "" {
				code = matcher.DetectFaultCode(msg)
			}
			return &matcher.FaultError{
				Code:    code,
				Message: msg,
				Driver:  call.Driver,
				Gateway: call.Gateway,
			}, true
		}
	}
	return nil, false
}

// ActiveStep returns the StepContext attached to ctx, falling back to the
// currently active step on kv when ctx carries no step (for libraries like GORM
// that call database/sql with context.Background()).
func (kv *Kavach) ActiveStep(ctx context.Context) (*StepContext, bool) {
	if sc, ok := StepFromContext(ctx); ok {
		return sc, true
	}
	if kv == nil {
		return nil, false
	}
	if sc := kv.activeStep.Load(); sc != nil && !sc.ended.Load() {
		return sc, true
	}
	return nil, false
}

// BeginStep starts a new step for in, attaches its *StepContext and deterministic
// clock.Provider to ctx, and records RecordType::Input when in ModeRecord.
func (kv *Kavach) BeginStep(ctx context.Context, in Input) (context.Context, *StepContext) {
	if ctx == nil {
		ctx = context.Background()
	}
	if kv == nil {
		return ctx, nil
	}
	seq := kv.stepSeq.Add(1)
	attempt := kv.nextAttempt(in)
	return kv.newStepContext(ctx, seq, attempt, in, nil, nil)
}

func (kv *Kavach) newStepContext(
	ctx context.Context,
	seq uint64,
	attempt int,
	in Input,
	hostEnv *host.Env,
	table *matcher.StepTable,
) (context.Context, *StepContext) {
	if ctx == nil {
		ctx = context.Background()
	}

	var inheritedClock clock.Provider
	if cp, ok := clock.FromContext(ctx); ok {
		inheritedClock = cp
	}

	sc := &StepContext{
		kv:        kv,
		seq:       seq,
		attempt:   attempt,
		input:     in,
		mode:      kv.mode,
		hostEnv:   hostEnv,
		table:     table,
		clockProv: inheritedClock,
	}

	if kv.mode == ModeRecord {
		data := kv.Scrub(cloneSlice(in.Data))
		sc.records = append(sc.records, ring.Record{
			Type:     ring.RecordInput,
			Source:   in.Source,
			Position: in.Position,
			Data:     data,
		})
	}

	kv.activeStep.Store(sc)
	ctx = context.WithValue(ctx, stepCtxKey{}, sc)
	ctx = clock.WithProvider(ctx, sc)
	return ctx, sc
}

// EndStep finalizes the step associated with ctx (or the active fallback step).
func (kv *Kavach) EndStep(ctx context.Context, err error) error {
	sc, ok := kv.ActiveStep(ctx)
	if !ok {
		return err
	}
	return sc.End(err)
}

// RecordGateway records a gateway call on the active step (resolving via ctx
// or kv's active-step fallback).
func (kv *Kavach) RecordGateway(ctx context.Context, call GatewayCall) {
	if sc, ok := kv.ActiveStep(ctx); ok {
		sc.RecordGateway(call)
	}
}

// ReplayGateway replays a gateway call on the active step (resolving via ctx
// or kv's active-step fallback).
func (kv *Kavach) ReplayGateway(ctx context.Context, call GatewayCall) ([]byte, error) {
	if kv == nil || kv.mode != ModeReplay {
		return nil, ErrNotReplaying
	}
	sc, ok := kv.ActiveStep(ctx)
	if !ok {
		return nil, ErrNoActiveStep
	}
	return sc.ReplayGateway(call)
}

// DoGateway executes or replays a gateway call cleanly in one call:
//   - ModeReplay: never calls liveFn; replays via ReplayGateway.
//   - ModeRecord: calls liveFn and records the result as RecordType::Gateway.
//   - ModeOff:    calls liveFn directly with zero overhead.
func (kv *Kavach) DoGateway(ctx context.Context, call GatewayCall, liveFn func() ([]byte, error)) ([]byte, error) {
	if kv == nil || kv.mode == ModeOff {
		if liveFn == nil {
			return nil, ErrNoActiveStep
		}
		return liveFn()
	}
	if sc, ok := kv.ActiveStep(ctx); ok {
		return sc.DoGateway(call, liveFn)
	}
	if kv.mode == ModeReplay {
		return nil, ErrNoActiveStep
	}
	if liveFn == nil {
		return nil, ErrNoActiveStep
	}
	return liveFn()
}

// RecordOutput records an output emission on the active step (resolving via ctx
// or kv's active-step fallback).
func (kv *Kavach) RecordOutput(ctx context.Context, out Output) {
	if sc, ok := kv.ActiveStep(ctx); ok {
		sc.RecordOutput(out)
	}
}

// Now returns the deterministic UTC timestamp for the active step, or time.Now().UTC().
func (kv *Kavach) Now(ctx context.Context) time.Time {
	if sc, ok := kv.ActiveStep(ctx); ok {
		return sc.Now()
	}
	return clock.Now(ctx)
}

// Rand returns a deterministic *clock.RNG bound to the active step.
func (kv *Kavach) Rand(ctx context.Context) *clock.RNG {
	if sc, ok := kv.ActiveStep(ctx); ok {
		return sc.Rand()
	}
	return clock.Rand(ctx)
}

// RunStepWithTable executes one step in-process against a pre-populated
// matcher.StepTable (and optional clock.Provider) without spawning a subprocess.
// Ideal for adapter unit tests and Lago L1 reordered-gateway / fault-injection tests.
func (kv *Kavach) RunStepWithTable(
	ctx context.Context,
	in Input,
	table *matcher.StepTable,
	runner StepRunner,
) (*StepContext, error) {
	if kv == nil {
		return nil, errors.New("kavach: nil Kavach instance")
	}
	if runner == nil {
		return nil, errors.New("kavach: nil StepRunner")
	}
	seq := kv.stepSeq.Add(1)
	attempt := kv.nextAttempt(in)
	stepCtx, sc := kv.newStepContext(ctx, seq, attempt, in, nil, table)
	runErr := runner.RunStep(stepCtx, in)
	endErr := sc.End(runErr)
	return sc, endErr
}

// ServeOrReplay runs handler normally in ModeOff / ModeRecord, or runs the
// deterministic SPEC §9 kavach-host stdio loop in ModeReplay.
//
// Accepted handler types:
//   - kavach.StepRunner (also checked for Server / Starter / http.Handler outside Replay)
//   - func(context.Context, kavach.Input) error
//   - http.Handler (replayed deterministically via httptest.ResponseRecorder in Replay mode)
//   - kavach.Server (`Serve(context.Context) error`) / kavach.Starter (`Start(context.Context)`)
func (kv *Kavach) ServeOrReplay(handler any) error {
	if kv == nil {
		return errors.New("kavach: nil Kavach instance")
	}
	if handler == nil {
		return errors.New("kavach: ServeOrReplay called with nil handler")
	}

	if kv.mode == ModeReplay {
		runner, err := asStepRunner(kv, handler)
		if err != nil {
			return err
		}
		return kv.ServeHost(runner)
	}

	// Normal production / dev runtime.
	switch h := handler.(type) {
	case Server:
		return h.Serve(context.Background())
	case Starter:
		h.Start(context.Background())
		return nil
	case http.Handler:
		return http.ListenAndServe(kv.cfg.ListenAddr, h)
	case func(context.Context) error:
		return h(context.Background())
	default:
		return fmt.Errorf("kavach: handler of type %T cannot be served in %s mode (implement http.Handler, kavach.Server, or kavach.Starter)", handler, kv.mode)
	}
}

// ServeHost runs the deterministic kavach-host stdio protocol loop directly for runner.
func (kv *Kavach) ServeHost(runner StepRunner) error {
	if kv == nil {
		return errors.New("kavach: nil Kavach instance")
	}
	if runner == nil {
		return errors.New("kavach: nil StepRunner")
	}
	in := kv.cfg.Stdin
	if in == nil {
		in = os.Stdin
	}
	out := kv.cfg.Stdout
	if out == nil {
		out = os.Stdout
	}

	opts := host.Options{
		SDKVersion:     "kavach-go/" + Version,
		InvariantNames: kv.InvariantNames,
		OnInit: func(service, start string, snapshot []byte) error {
			kv.resetSession()
			if kv.cfg.OnReset != nil {
				return kv.cfg.OnReset()
			}
			return nil
		},
		RunStep: func(stepIn host.StepInput, env *host.Env) host.StepOutcome {
			in := Input{
				Source:   stepIn.Source,
				Position: stepIn.Position,
				Data:     stepIn.Data,
			}
			seq := stepIn.Seq
			if seq == 0 {
				seq = kv.stepSeq.Add(1)
			}
			attempt := kv.nextAttempt(in)
			stepCtx, sc := kv.newStepContext(context.Background(), seq, attempt, in, env, nil)

			var runErr error
			panicked := false
			var panicVal any

			func() {
				defer func() {
					if v := recover(); v != nil {
						panicked = true
						panicVal = v
					}
				}()
				runErr = runner.RunStep(stepCtx, in)
			}()

			if panicked {
				// Let host.Serve classify abortStep / hostFatal / handler panic,
				// while making sure kv.activeStep is cleared cleanly.
				sc.mu.Lock()
				sc.panicked = true
				sc.ended.Store(true)
				sc.mu.Unlock()
				kv.activeStep.CompareAndSwap(sc, nil)
				panic(panicVal)
			}

			endErr := sc.End(runErr)
			if endErr != nil {
				var ie *InvariantError
				if errors.As(endErr, &ie) {
					detail := ""
					if ie.Err != nil {
						detail = ie.Err.Error()
					}
					return host.StepOutcome{
						Err:             runErr,
						InvariantName:   ie.Name,
						InvariantDetail: detail,
					}
				}
				return host.StepOutcome{Err: endErr}
			}
			return host.StepOutcome{}
		},
	}

	return host.Serve(opts, in, out)
}

func (kv *Kavach) resetSession() {
	kv.mu.Lock()
	kv.eval.Reset()
	kv.attempts = make(map[string]int)
	for _, r := range kv.faultRules {
		r.Reset()
	}
	kv.mu.Unlock()
	kv.stepSeq.Store(0)
	kv.activeStep.Store(nil)
}

func (kv *Kavach) nextAttempt(in Input) int {
	fp := inputFingerprint(in)
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.attempts[fp]++
	return kv.attempts[fp]
}

func asStepRunner(kv *Kavach, handler any) (StepRunner, error) {
	// Check StepRunner FIRST so adapters like kavachhttp.Wrap (which implement
	// both http.Handler and StepRunner) use their exact adapter replay logic.
	if r, ok := handler.(StepRunner); ok {
		return r, nil
	}
	if fn, ok := handler.(func(context.Context, Input) error); ok {
		return StepFunc(fn), nil
	}
	if h, ok := handler.(http.Handler); ok {
		return &httpFallbackRunner{kv: kv, h: h}, nil
	}
	return nil, fmt.Errorf("kavach: handler of type %T does not implement StepRunner, func(context.Context, Input) error, or http.Handler", handler)
}

// httpFallbackRunner replays an unwrapped http.Handler deterministically via
// httptest.ResponseRecorder without opening any TCP socket.
type httpFallbackRunner struct {
	kv *Kavach
	h  http.Handler
}

type httpEnvelope struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

func (r *httpFallbackRunner) RunStep(ctx context.Context, in Input) error {
	method, path := http.MethodPost, "/"
	if rest, ok := strings.CutPrefix(in.Source, "http:"); ok {
		if parts := strings.SplitN(strings.TrimSpace(rest), " ", 2); len(parts) == 2 {
			method, path = parts[0], parts[1]
		} else if rest != "" {
			path = rest
		}
	}
	body := in.Data
	headers := in.Headers

	var env httpEnvelope
	if len(in.Data) > 0 && in.Data[0] == '{' && json.Unmarshal(in.Data, &env) == nil && (env.Method != "" || env.Path != "" || env.URL != "") {
		if env.Method != "" {
			method = env.Method
		}
		if env.Path != "" {
			path = env.Path
		} else if env.URL != "" {
			path = env.URL
		}
		if len(env.Headers) > 0 {
			headers = env.Headers
		}
		if env.Body != nil {
			body = env.Body
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)

	respPayload, _ := json.Marshal(struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers,omitempty"`
		Body    string            `json:"body,omitempty"`
	}{
		Status:  rec.Code,
		Headers: flattenHeaders(rec.Header()),
		Body:    rec.Body.String(),
	})
	r.kv.RecordOutput(ctx, Output{Sink: "http:response", Data: respPayload})
	if rec.Code >= 500 {
		return fmt.Errorf("http %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return nil
}

func flattenHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) > 0 {
			out[k] = strings.Join(vals, ", ")
		}
	}
	return out
}

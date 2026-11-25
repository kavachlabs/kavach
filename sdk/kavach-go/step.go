package kavach

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kavachlabs/kavach-go/clock"
	"github.com/kavachlabs/kavach-go/host"
	"github.com/kavachlabs/kavach-go/invariants"
	"github.com/kavachlabs/kavach-go/matcher"
	"github.com/kavachlabs/kavach-go/ring"
)

// Type aliases so adapters only need to import github.com/kavachlabs/kavach-go.
type (
	GatewayCall           = matcher.GatewayCall
	FaultRule             = matcher.FaultRule
	FaultError            = matcher.FaultError
	UnmatchedGatewayError = matcher.UnmatchedGatewayError
	MatchTier             = matcher.MatchTier
)

// Well-known driver-level fault codes re-exported from matcher.
const (
	CodeRedisLoading  = matcher.CodeRedisLoading
	CodePG40001       = matcher.CodePG40001
	CodeETIMEDOUT     = matcher.CodeETIMEDOUT
	FaultRedisLoading = matcher.FaultRedisLoading
	FaultPG40001      = matcher.FaultPG40001
	FaultETIMEDOUT    = matcher.FaultETIMEDOUT
)

// ErrNotReplaying is returned when ReplayGateway is called outside ModeReplay.
var ErrNotReplaying = errors.New("kavach: ReplayGateway called outside replay mode")

// ErrNoActiveStep is returned when a replay operation runs with no active step.
var ErrNoActiveStep = errors.New("kavach: no active step in context")

// Input describes one inbound event/request that begins a Kavach step.
type Input struct {
	Source   string            `json:"source"`             // e.g. "http:POST /events", "kafka:raw_events"
	Position string            `json:"position,omitempty"` // e.g. "0:42" (partition:offset) or request ID
	Key      []byte            `json:"key,omitempty"`      // optional partition/entity key
	Headers  map[string]string `json:"headers,omitempty"`  // optional headers/metadata
	Data     []byte            `json:"data"`               // raw payload bytes
}

// Output describes one outbound side effect emitted by a handler during a step.
type Output struct {
	Sink  string `json:"sink"`            // e.g. "kafka:events_charged_in_advance", "http:response", "pg:exec"
	Key   string `json:"key,omitempty"`   // optional routing/idempotency key
	Data  []byte `json:"data"`            // payload bytes
	Scope uint8  `json:"scope,omitempty"` // 0 = remote (default), 1 = local
}

// Step is the read-only step view passed to registered code invariants.
type Step interface {
	Seq() uint64
	Input() Input
	Outputs() []Output
	Error() error
	Attempt() int
	AssertAtMostOnceEmit(sink, jsonPathKey string) error
}

type stepCtxKey struct{}

// StepFromContext extracts the *StepContext attached to ctx, if any.
func StepFromContext(ctx context.Context) (*StepContext, bool) {
	if ctx == nil {
		return nil, false
	}
	sc, ok := ctx.Value(stepCtxKey{}).(*StepContext)
	return sc, ok && sc != nil
}

// StepContext tracks the lifecycle, recorded/replayed reads, gateway calls, and
// emitted outputs of a single step. All methods are safe for concurrent use by
// goroutines spawned inside a step (e.g. errgroup.Group).
type StepContext struct {
	kv      *Kavach
	seq     uint64
	attempt int
	input   Input
	mode    Mode

	mu       sync.Mutex
	records  []ring.Record // buffered frames for atomic step commit in ModeRecord
	outputs  []Output
	err      error
	panicked bool
	ended    atomic.Bool
	endOnce  sync.Once
	endErr   error

	hostEnv   *host.Env
	table     *matcher.StepTable
	clockProv clock.Provider
}

// Seq returns the step sequence number.
func (s *StepContext) Seq() uint64 { return s.seq }

// Attempt returns the 1-based delivery attempt number for this input within the session.
func (s *StepContext) Attempt() int { return s.attempt }

// Input returns the step's inbound Input.
func (s *StepContext) Input() Input { return s.input }

// Mode returns the Kavach mode active for this step.
func (s *StepContext) Mode() Mode { return s.mode }

// Error returns the error recorded on EndStep (nil if the step succeeded or has not ended).
func (s *StepContext) Error() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Outputs returns a snapshot copy of all outputs emitted during this step.
func (s *StepContext) Outputs() []Output {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Output, len(s.outputs))
	for i, o := range s.outputs {
		out[i] = Output{Sink: o.Sink, Key: o.Key, Data: cloneSlice(o.Data), Scope: o.Scope}
	}
	return out
}

// Table returns the per-step matcher.StepTable if one is attached (in-process replay).
func (s *StepContext) Table() *matcher.StepTable { return s.table }

// Now implements clock.Provider.
func (s *StepContext) Now() time.Time {
	if s.ended.Load() {
		return time.Now().UTC()
	}
	if s.mode == ModeReplay {
		if s.hostEnv != nil {
			return s.hostEnv.Now()
		}
		if s.clockProv != nil {
			return s.clockProv.Now().UTC()
		}
		return time.Unix(0, 0).UTC()
	}
	ns := time.Now().UTC().UnixNano()
	if s.mode == ModeRecord {
		s.mu.Lock()
		if !s.ended.Load() {
			s.records = append(s.records, ring.Record{
				Type:      ring.RecordClock,
				UnixNanos: ns,
			})
		}
		s.mu.Unlock()
	}
	return time.Unix(0, ns).UTC()
}

// ReadRand implements clock.Provider.
func (s *StepContext) ReadRand(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.ended.Load() {
		return io.ReadFull(cryptorand.Reader, p)
	}
	if s.mode == ModeReplay {
		if s.hostEnv != nil {
			return s.hostEnv.ReadRand(p)
		}
		if s.clockProv != nil {
			return s.clockProv.ReadRand(p)
		}
		clock.DeterministicBytes(s.seq, 1, p)
		return len(p), nil
	}
	n, err := io.ReadFull(cryptorand.Reader, p)
	if err != nil {
		return n, err
	}
	if s.mode == ModeRecord {
		cp := cloneSlice(p[:n])
		s.mu.Lock()
		if !s.ended.Load() {
			s.records = append(s.records, ring.Record{
				Type: ring.RecordRand,
				Data: cp,
			})
		}
		s.mu.Unlock()
	}
	return n, nil
}

// Rand returns a deterministic *clock.RNG backed by this step.
func (s *StepContext) Rand() *clock.RNG {
	return clock.NewRNG(s)
}

// RecordGateway records a completed outbound gateway call in ModeRecord (after
// applying configured PII scrubbing). It is a no-op outside ModeRecord.
func (s *StepContext) RecordGateway(call GatewayCall) {
	if s == nil || s.mode != ModeRecord || s.ended.Load() {
		return
	}
	c := call.Canonicalize()
	req := s.kv.Scrub(c.Request)
	resp := s.kv.Scrub(c.Response)
	errStr := c.Error
	if errStr != "" && s.kv.cfg.PII != ScrubOff {
		errStr = ScrubString(s.kv.cfg.PII, s.kv.cfg.PIIKey, "error", errStr)
	}
	s.mu.Lock()
	if !s.ended.Load() {
		s.records = append(s.records, ring.Record{
			Type:     ring.RecordGateway,
			Flags:    ring.FlagCritical,
			Gateway:  c.Gateway,
			Request:  req,
			Response: resp,
			Error:    errStr,
			Scope:    c.Scope,
		})
	}
	s.mu.Unlock()
}

// ReplayGateway resolves an outbound gateway call during ModeReplay without
// touching any live DB, Redis, or network socket.
func (s *StepContext) ReplayGateway(call GatewayCall) ([]byte, error) {
	if s == nil {
		return nil, ErrNoActiveStep
	}
	if s.mode != ModeReplay {
		return nil, ErrNotReplaying
	}
	c := call.Canonicalize()

	// Check session-scoped fault rules registered via kv.InjectFault.
	if fe, ok := s.kv.matchFault(c); ok {
		return nil, fe
	}

	if s.table != nil {
		resp, err, _ := s.table.MatchGateway(c)
		return resp, err
	}
	if s.hostEnv != nil {
		return s.hostEnv.Query(c.Gateway, c.Request)
	}
	return nil, &matcher.UnmatchedGatewayError{
		Driver:       c.Driver,
		Gateway:      c.Gateway,
		Op:           c.Op,
		NormalizedOp: c.NormalizedOp,
		KeyArgs:      c.KeyArgs,
		StepSeq:      s.seq,
	}
}

// DoGateway is a convenience hook for driver adapters:
//   - ModeReplay: returns ReplayGateway(call) without invoking liveFn.
//   - ModeRecord: invokes liveFn, records the Gateway call (with response/error), and returns.
//   - ModeOff:    invokes liveFn directly.
func (s *StepContext) DoGateway(call GatewayCall, liveFn func() ([]byte, error)) ([]byte, error) {
	if s == nil {
		if liveFn == nil {
			return nil, ErrNoActiveStep
		}
		return liveFn()
	}
	if s.mode == ModeReplay {
		return s.ReplayGateway(call)
	}
	if liveFn == nil {
		return nil, errors.New("kavach: liveFn is nil")
	}
	resp, err := liveFn()
	if s.mode == ModeRecord {
		rec := call
		if err != nil {
			rec.Error = err.Error()
		} else {
			rec.Response = resp
		}
		s.RecordGateway(rec)
	}
	return resp, err
}

// RecordOutput records an outbound side effect (Kafka produce, SQL write, HTTP
// response). In ModeRecord it appends a RecordType::Output frame; in ModeReplay
// it captures the emit into the StepTable / host protocol without touching any
// real sink.
func (s *StepContext) RecordOutput(out Output) {
	if s == nil || s.ended.Load() {
		return
	}
	data := cloneSlice(out.Data)
	key := out.Key
	if s.mode == ModeRecord {
		data = s.kv.Scrub(data)
		if key != "" && s.kv.cfg.PII != ScrubOff {
			key = ScrubString(s.kv.cfg.PII, s.kv.cfg.PIIKey, "key", key)
		}
	}
	stored := Output{Sink: out.Sink, Key: key, Data: data, Scope: out.Scope}

	s.mu.Lock()
	if s.ended.Load() {
		s.mu.Unlock()
		return
	}
	s.outputs = append(s.outputs, stored)
	if s.mode == ModeRecord {
		s.records = append(s.records, ring.Record{
			Type:  ring.RecordOutput,
			Sink:  stored.Sink,
			Data:  stored.Data,
			Scope: stored.Scope,
		})
	}
	s.mu.Unlock()

	if s.mode == ModeReplay {
		if s.table != nil {
			s.table.RecordOutput(matcher.OutputRecord{
				Sink:  stored.Sink,
				Key:   stored.Key,
				Data:  stored.Data,
				Scope: stored.Scope,
			})
		}
		if s.hostEnv != nil {
			s.hostEnv.Emit(stored.Sink, stored.Data)
		}
	}
}

// Config reads a configuration value deterministically (recording in ModeRecord,
// replaying via hostEnv in ModeReplay).
func (s *StepContext) Config(key string, liveFn func(string) ([]byte, string, bool)) ([]byte, bool) {
	if s != nil && s.mode == ModeReplay && s.hostEnv != nil {
		return s.hostEnv.Config(key)
	}
	if liveFn == nil {
		return nil, false
	}
	v, src, ok := liveFn(key)
	if s != nil && s.mode == ModeRecord && !s.ended.Load() {
		s.mu.Lock()
		if !s.ended.Load() {
			s.records = append(s.records, ring.Record{
				Type:    ring.RecordConfig,
				Flags:   ring.FlagCritical,
				Key:     key,
				Present: ok,
				Value:   s.kv.Scrub(cloneSlice(v)),
				Source:  src,
			})
		}
		s.mu.Unlock()
	}
	return v, ok
}

// AssertAtMostOnceEmit implements Step. It verifies that no key extracted from
// outputs to sink via jsonPathKey is emitted more than once across the session
// (including retries), and fails if a failing step leaked an emit to sink.
func (s *StepContext) AssertAtMostOnceEmit(sink, jsonPathKey string) error {
	if s == nil || s.kv == nil {
		return nil
	}
	snap := s.snapshot()
	return s.kv.eval.AssertAtMostOnceEmit(sink, jsonPathKey, snap)
}

func (s *StepContext) snapshot() invariants.StepSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	outs := make([]invariants.Output, len(s.outputs))
	for i, o := range s.outputs {
		outs[i] = invariants.Output{Sink: o.Sink, Key: o.Key, Data: o.Data}
	}
	var base []invariants.Output
	if s.table != nil {
		rec := s.table.RecordedOutputs()
		base = make([]invariants.Output, len(rec))
		for i, o := range rec {
			base[i] = invariants.Output{Sink: o.Sink, Key: o.Key, Data: o.Data}
		}
	}
	return invariants.StepSnapshot{
		Seq:             s.seq,
		Attempt:         s.attempt,
		Outputs:         outs,
		BaselineOutputs: base,
		Err:             s.err,
		Panicked:        s.panicked,
	}
}

// End finalizes the step, evaluates invariants, flushes buffered records
// atomically to the KVRING02 ring in ModeRecord, and clears the active step.
// Calling End more than once is a safe no-op returning the first result.
func (s *StepContext) End(err error) error {
	if s == nil {
		return err
	}
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.ended.Store(true)
		s.mu.Unlock()

		// Release fallback active step pointer.
		s.kv.activeStep.CompareAndSwap(s, nil)

		invErr := s.kv.checkInvariants(s)

		if s.mode == ModeRecord && s.kv.writer != nil {
			s.mu.Lock()
			recs := append([]ring.Record(nil), s.records...)
			s.mu.Unlock()
			switch {
			case s.panicked:
				recs = append(recs, ring.Record{
					Type:    ring.RecordMarker,
					Flags:   ring.FlagCritical,
					Kind:    ring.MarkerPanic,
					Message: fmt.Sprint(err),
				})
			case err != nil:
				recs = append(recs, ring.Record{
					Type:    ring.RecordMarker,
					Flags:   ring.FlagCritical,
					Kind:    ring.MarkerError,
					Message: err.Error(),
				})
			case invErr != nil:
				var ie *InvariantError
				name, detail := "invariant", invErr.Error()
				if errors.As(invErr, &ie) {
					name, detail = ie.Name, ie.Err.Error()
				}
				recs = append(recs, ring.Record{
					Type:    ring.RecordMarker,
					Flags:   ring.FlagCritical,
					Kind:    ring.MarkerInvariant,
					Message: name,
					Data:    []byte(detail),
				})
			}
			_ = s.kv.writer.WriteStep(recs, true)
		}

		if invErr != nil {
			s.endErr = invErr
		} else {
			s.endErr = err
		}
	})
	return s.endErr
}

// Package-level context helpers (convenient when adapters hold only ctx).

// Now returns the deterministic UTC timestamp for the step in ctx, falling back
// to clock.Now(ctx) / time.Now().UTC().
func Now(ctx context.Context) time.Time {
	if sc, ok := StepFromContext(ctx); ok {
		return sc.Now()
	}
	return clock.Now(ctx)
}

// Rand returns the deterministic *clock.RNG for the step in ctx.
func Rand(ctx context.Context) *clock.RNG {
	if sc, ok := StepFromContext(ctx); ok {
		return sc.Rand()
	}
	return clock.Rand(ctx)
}

// RecordGateway records a gateway call on the StepContext carried by ctx.
func RecordGateway(ctx context.Context, call GatewayCall) {
	if sc, ok := StepFromContext(ctx); ok {
		sc.RecordGateway(call)
	}
}

// ReplayGateway replays a gateway call via the StepContext carried by ctx.
func ReplayGateway(ctx context.Context, call GatewayCall) ([]byte, error) {
	if sc, ok := StepFromContext(ctx); ok {
		return sc.ReplayGateway(call)
	}
	return nil, ErrNoActiveStep
}

// RecordOutput records an output emission on the StepContext carried by ctx.
func RecordOutput(ctx context.Context, out Output) {
	if sc, ok := StepFromContext(ctx); ok {
		sc.RecordOutput(out)
	}
}

func inputFingerprint(in Input) string {
	h := sha256.New()
	h.Write([]byte(in.Source))
	h.Write([]byte{0})
	h.Write([]byte(in.Position))
	h.Write([]byte{0})
	h.Write(in.Data)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func cloneSlice(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

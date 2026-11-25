package matcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// MatchTier classifies how a Gateway call was resolved during replay.
type MatchTier uint8

const (
	TierStrictSeq MatchTier = iota
	TierKeyedTemplate
	TierFaultInjected
	TierUnmatched
)

// String returns the canonical telemetry name for the match tier.
func (t MatchTier) String() string {
	switch t {
	case TierStrictSeq:
		return "strict_seq"
	case TierKeyedTemplate:
		return "keyed_template"
	case TierFaultInjected:
		return "fault_injected"
	case TierUnmatched:
		return "unmatched_gateway"
	default:
		return "unknown"
	}
}

// GatewayCall represents one outbound read/RPC call at a driver boundary.
type GatewayCall struct {
	Driver        string   `json:"driver"`
	Gateway       string   `json:"gateway,omitempty"`
	Op            string   `json:"op,omitempty"`
	NormalizedOp  string   `json:"normalized_op,omitempty"`
	OpFingerprint string   `json:"op_fingerprint,omitempty"`
	KeyArgs       []string `json:"key_args,omitempty"`
	Request       []byte   `json:"request,omitempty"`
	Response      []byte   `json:"response,omitempty"`
	Error         string   `json:"error,omitempty"`
	Scope         uint8    `json:"scope,omitempty"`
}

// Canonicalize fills derived Driver, NormalizedOp, Gateway, OpFingerprint, and
// deterministic Request bytes without mutating caller slices.
func (g GatewayCall) Canonicalize() GatewayCall {
	out := g
	out.Driver = strings.ToLower(strings.TrimSpace(out.Driver))
	if out.Driver == "" && out.Gateway != "" {
		if idx := strings.IndexByte(out.Gateway, ':'); idx > 0 {
			out.Driver = strings.ToLower(strings.TrimSpace(out.Gateway[:idx]))
			if out.Op == "" {
				out.Op = strings.TrimSpace(out.Gateway[idx+1:])
			}
		} else {
			out.Driver = strings.ToLower(strings.TrimSpace(out.Gateway))
		}
	}
	if out.NormalizedOp == "" && out.Op != "" {
		out.NormalizedOp = NormalizeOp(out.Driver, out.Op)
	} else if out.NormalizedOp != "" {
		out.NormalizedOp = strings.TrimSpace(out.NormalizedOp)
	}
	if out.Gateway == "" {
		switch {
		case out.Driver != "" && (out.Driver == "pg" || out.Driver == "sql" || out.Driver == "postgres"):
			out.Gateway = out.Driver + ":" + SQLKind(out.NormalizedOp)
		case out.Driver == "redis" && out.NormalizedOp != "":
			out.Gateway = "redis:" + strings.ToLower(out.NormalizedOp)
		case out.Driver != "" && out.NormalizedOp != "":
			out.Gateway = out.Driver + ":" + out.NormalizedOp
		default:
			out.Gateway = out.Driver
		}
	}
	if len(out.KeyArgs) > 0 {
		cp := make([]string, len(out.KeyArgs))
		copy(cp, out.KeyArgs)
		out.KeyArgs = cp
	}
	if out.OpFingerprint == "" {
		out.OpFingerprint = ComputeFingerprint(out.Driver, out.NormalizedOp, out.KeyArgs)
	}
	if len(out.Request) == 0 {
		b, _ := json.Marshal(canonicalReqEnvelope{
			Driver:        out.Driver,
			NormalizedOp:  out.NormalizedOp,
			KeyArgs:       out.KeyArgs,
			OpFingerprint: out.OpFingerprint,
		})
		out.Request = b
	} else {
		out.Request = cloneBytes(out.Request)
	}
	out.Response = cloneBytes(out.Response)
	return out
}

// OutputRecord represents one outbound side effect emitted during a step.
type OutputRecord struct {
	Sink  string `json:"sink"`
	Key   string `json:"key,omitempty"`
	Data  []byte `json:"data"`
	Scope uint8  `json:"scope,omitempty"`
}

// TelemetryNote records non-fatal matcher events (such as reordered_gateway).
type TelemetryNote struct {
	Kind        string `json:"kind"`
	Driver      string `json:"driver"`
	Gateway     string `json:"gateway"`
	Normalized  string `json:"normalized_op,omitempty"`
	RecordedIdx int    `json:"recorded_idx,omitempty"`
	CallIdx     int    `json:"call_idx,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

type recordedGateway struct {
	call     GatewayCall
	consumed bool
}

// StepTable maintains separate Gateway and Output tables for a single step so
// reordering a Gateway check ahead of an Output emit never triggers a false
// divergence, while still catching unmatched reads and leaked emits.
type StepTable struct {
	mu              sync.Mutex
	stepSeq         uint64
	gateways        []recordedGateway
	recordedOutputs []OutputRecord
	emittedOutputs  []OutputRecord
	faults          []*FaultRule
	seqCursor       int
	callCount       int
	reorderedCount  int
	telemetry       []TelemetryNote
}

// NewStepTable builds a per-step matcher table with separate Gateway and Output streams.
func NewStepTable(stepSeq uint64, recordedGateways []GatewayCall, recordedOutputs []OutputRecord) *StepTable {
	gws := make([]recordedGateway, len(recordedGateways))
	for i, g := range recordedGateways {
		gws[i] = recordedGateway{call: g.Canonicalize()}
	}
	outs := make([]OutputRecord, len(recordedOutputs))
	for i, o := range recordedOutputs {
		outs[i] = OutputRecord{Sink: o.Sink, Key: o.Key, Data: cloneBytes(o.Data), Scope: o.Scope}
	}
	return &StepTable{
		stepSeq:         stepSeq,
		gateways:        gws,
		recordedOutputs: outs,
	}
}

// InjectFault registers a driver-level fault rule on this step table.
func (t *StepTable) InjectFault(rule FaultRule) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := rule
	t.faults = append(t.faults, &r)
}

// MatchGateway resolves an outbound gateway call against this step's recorded
// gateway table using two-tier matching:
//  1. Primary (StrictSeq): next unconsumed slot matching (driver, op_fingerprint).
//  2. Fallback (KeyedTemplate): any unconsumed slot matching (driver, normalized_op, key_args),
//     recording a non-fatal "reordered_gateway" telemetry note.
//  3. Hard fail: returns *UnmatchedGatewayError with actionable diagnostics.
func (t *StepTable) MatchGateway(req GatewayCall) ([]byte, error, MatchTier) {
	t.mu.Lock()
	defer t.mu.Unlock()

	call := req.Canonicalize()
	callIdx := t.callCount
	t.callCount++

	// Check injected driver-level faults first.
	for _, rule := range t.faults {
		if rule.matches(call) {
			msg := ExpandFault(rule.ErrString)
			code := DetectFaultCode(rule.ErrString)
			if code == "" {
				code = DetectFaultCode(msg)
			}
			t.telemetry = append(t.telemetry, TelemetryNote{
				Kind:       "fault_injected",
				Driver:     call.Driver,
				Gateway:    call.Gateway,
				Normalized: call.NormalizedOp,
				CallIdx:    callIdx,
				Detail:     msg,
			})
			return nil, &FaultError{
				Code:    code,
				Message: msg,
				Driver:  call.Driver,
				Gateway: call.Gateway,
			}, TierFaultInjected
		}
	}

	// Advance StrictSeq cursor past slots already consumed by KeyedTemplate fallback.
	for t.seqCursor < len(t.gateways) && t.gateways[t.seqCursor].consumed {
		t.seqCursor++
	}

	// Tier 1: StrictSeq on (driver, op_fingerprint) or exact (gateway, request).
	if t.seqCursor < len(t.gateways) {
		rec := &t.gateways[t.seqCursor]
		if strictMatch(rec.call, call) {
			rec.consumed = true
			t.seqCursor++
			return gatewayResult(rec.call), gatewayErr(rec.call), TierStrictSeq
		}
	}

	// Tier 2: KeyedTemplate fallback on (driver, normalized_op, key_args).
	for i := range t.gateways {
		if t.gateways[i].consumed {
			continue
		}
		if keyedTemplateMatch(t.gateways[i].call, call) {
			t.gateways[i].consumed = true
			t.reorderedCount++
			t.telemetry = append(t.telemetry, TelemetryNote{
				Kind:        "reordered_gateway",
				Driver:      call.Driver,
				Gateway:     call.Gateway,
				Normalized:  call.NormalizedOp,
				RecordedIdx: i,
				CallIdx:     callIdx,
				Detail:      fmt.Sprintf("matched recorded gateway #%d out of order via KeyedTemplate", i),
			})
			return gatewayResult(t.gateways[i].call), gatewayErr(t.gateways[i].call), TierKeyedTemplate
		}
	}

	// Tier 3: Hard fail with clear UnmatchedGateway diagnostics.
	avail := make([]string, 0, len(t.gateways))
	for i, g := range t.gateways {
		if g.consumed {
			continue
		}
		avail = append(avail, fmt.Sprintf("#%d(%s %q keys=%v)", i, g.call.Gateway, g.call.NormalizedOp, g.call.KeyArgs))
	}
	return nil, &UnmatchedGatewayError{
		Driver:       call.Driver,
		Gateway:      call.Gateway,
		Op:           call.Op,
		NormalizedOp: call.NormalizedOp,
		KeyArgs:      call.KeyArgs,
		StepSeq:      t.stepSeq,
		CallIndex:    callIdx,
		Available:    avail,
	}, TierUnmatched
}

func strictMatch(rec, call GatewayCall) bool {
	if !driverCompatible(rec, call) {
		return false
	}
	if rec.OpFingerprint != "" && call.OpFingerprint != "" && rec.OpFingerprint == call.OpFingerprint {
		return true
	}
	if rec.Gateway == call.Gateway && len(rec.Request) > 0 && bytes.Equal(rec.Request, call.Request) {
		return true
	}
	return false
}

func keyedTemplateMatch(rec, call GatewayCall) bool {
	if !driverCompatible(rec, call) {
		return false
	}
	if rec.NormalizedOp != "" && call.NormalizedOp != "" {
		if rec.NormalizedOp != call.NormalizedOp {
			return false
		}
		return slices.Equal(rec.KeyArgs, call.KeyArgs)
	}
	if rec.Gateway != "" && rec.Gateway == call.Gateway {
		return slices.Equal(rec.KeyArgs, call.KeyArgs)
	}
	return false
}

func driverCompatible(rec, call GatewayCall) bool {
	if rec.Driver != "" && call.Driver != "" {
		return strings.EqualFold(rec.Driver, call.Driver)
	}
	return strings.EqualFold(rec.Gateway, call.Gateway)
}

func gatewayResult(rec GatewayCall) []byte {
	if rec.Error != "" {
		return nil
	}
	return cloneBytes(rec.Response)
}

func gatewayErr(rec GatewayCall) error {
	if rec.Error == "" {
		return nil
	}
	code := DetectFaultCode(rec.Error)
	if code != "" {
		return &FaultError{Code: code, Message: rec.Error, Driver: rec.Driver, Gateway: rec.Gateway}
	}
	return errors.New(rec.Error)
}

// RecordOutput appends an emitted side effect into the step's Output table
// (completely independent of the Gateway table).
func (t *StepTable) RecordOutput(out OutputRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emittedOutputs = append(t.emittedOutputs, OutputRecord{
		Sink:  out.Sink,
		Key:   out.Key,
		Data:  cloneBytes(out.Data),
		Scope: out.Scope,
	})
}

// EmittedOutputs returns a snapshot copy of outputs emitted during replay.
func (t *StepTable) EmittedOutputs() []OutputRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]OutputRecord, len(t.emittedOutputs))
	for i, o := range t.emittedOutputs {
		out[i] = OutputRecord{Sink: o.Sink, Key: o.Key, Data: cloneBytes(o.Data), Scope: o.Scope}
	}
	return out
}

// RecordedOutputs returns the recorded baseline outputs for this step.
func (t *StepTable) RecordedOutputs() []OutputRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]OutputRecord, len(t.recordedOutputs))
	for i, o := range t.recordedOutputs {
		out[i] = OutputRecord{Sink: o.Sink, Key: o.Key, Data: cloneBytes(o.Data), Scope: o.Scope}
	}
	return out
}

// ReorderedCount returns how many gateway calls matched via KeyedTemplate fallback.
func (t *StepTable) ReorderedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reorderedCount
}

// Telemetry returns a copy of non-fatal matcher telemetry notes recorded in this step.
func (t *StepTable) Telemetry() []TelemetryNote {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TelemetryNote, len(t.telemetry))
	copy(out, t.telemetry)
	return out
}

// UnconsumedGateways returns any recorded gateway calls not consumed during replay.
func (t *StepTable) UnconsumedGateways() []GatewayCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []GatewayCall
	for _, g := range t.gateways {
		if !g.consumed {
			out = append(out, g.call)
		}
	}
	return out
}

// CompareOutputs compares emitted outputs against recorded baseline outputs at StepEnd.
func (t *StepTable) CompareOutputs() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	want := t.recordedOutputs
	got := t.emittedOutputs
	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			return fmt.Errorf("output %d missing: recorded sink=%q", i, want[i].Sink)
		case i >= len(want):
			return fmt.Errorf("output %d unexpected: emitted sink=%q", i, got[i].Sink)
		case want[i].Sink != got[i].Sink || !bytes.Equal(want[i].Data, got[i].Data):
			return fmt.Errorf("output %d differs: recorded (%s %q), replay (%s %q)",
				i, want[i].Sink, want[i].Data, got[i].Sink, got[i].Data)
		}
	}
	return nil
}

// CompareOutputsMultiset compares emitted vs recorded outputs independent of
// emission order (useful when concurrent errgroup goroutines emit to sinks).
func (t *StepTable) CompareOutputsMultiset() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	want := t.recordedOutputs
	got := t.emittedOutputs
	if len(want) != len(got) {
		return fmt.Errorf("output count differs: recorded %d, replay %d", len(want), len(got))
	}
	matched := make([]bool, len(got))
	for i, w := range want {
		found := false
		for j, g := range got {
			if !matched[j] && w.Sink == g.Sink && bytes.Equal(w.Data, g.Data) {
				matched[j] = true
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("output %d missing in multiset comparison: recorded (%s %q)", i, w.Sink, w.Data)
		}
	}
	return nil
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

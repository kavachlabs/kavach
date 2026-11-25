package invariants

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

// Output is a sink emission evaluated by declarative and code invariants.
type Output struct {
	Sink string `json:"sink"`
	Key  string `json:"key,omitempty"`
	Data []byte `json:"data"`
}

// StepSnapshot holds the outputs and outcome of a completed step.
type StepSnapshot struct {
	Seq             uint64
	Attempt         int
	Outputs         []Output
	BaselineOutputs []Output
	Err             error
	Panicked        bool
}

// Violation reports a broken invariant with its name, kind, and diagnostic detail.
type Violation struct {
	Name   string
	Kind   string
	Detail string
}

// Error implements error.
func (v *Violation) Error() string {
	if v.Detail == "" {
		return fmt.Sprintf("invariant %s violated", v.Name)
	}
	return fmt.Sprintf("invariant %s violated: %s", v.Name, v.Detail)
}

// Evaluator evaluates declarative YAML rules and session-wide assertions
// (such as AssertAtMostOnceEmit) across all steps of a run.
type Evaluator struct {
	mu             sync.Mutex
	rules          []Rule
	seenByRule     map[string]map[string]uint64 // ruleKey -> keyVal -> first stepSeq
	emittedBySink  map[string]int
	baselineBySink map[string]int
}

// NewEvaluator constructs an Evaluator for the given declarative rules.
func NewEvaluator(rules []Rule) *Evaluator {
	cp := make([]Rule, len(rules))
	copy(cp, rules)
	return &Evaluator{
		rules:          cp,
		seenByRule:     make(map[string]map[string]uint64),
		emittedBySink:  make(map[string]int),
		baselineBySink: make(map[string]int),
	}
}

// Reset clears per-session state (seen keys and output counters) between replay runs.
func (e *Evaluator) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seenByRule = make(map[string]map[string]uint64)
	e.emittedBySink = make(map[string]int)
}

// Rules returns a copy of the registered declarative rules.
func (e *Evaluator) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// RuleNames returns the names of all registered declarative rules.
func (e *Evaluator) RuleNames() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, len(e.rules))
	for i, r := range e.rules {
		names[i] = r.Name
	}
	return names
}

// SetBaselineCounts sets expected per-sink baseline emit counts for anti_lobotomy rules.
func (e *Evaluator) SetBaselineCounts(counts map[string]int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.baselineBySink = make(map[string]int, len(counts))
	for k, v := range counts {
		e.baselineBySink[k] = v
	}
}

// SinkMatches matches an output sink against a rule sink specification.
// Exact matches always succeed. When ruleSink has no prefix (no ':'), it also
// matches "<scheme>:<ruleSink>" (e.g., "events_charged_in_advance" matches
// "kafka:events_charged_in_advance").
func SinkMatches(outSink, ruleSink string) bool {
	if ruleSink == "" || ruleSink == "*" {
		return true
	}
	if outSink == ruleSink {
		return true
	}
	if !strings.Contains(ruleSink, ":") {
		if idx := strings.IndexByte(outSink, ':'); idx >= 0 && outSink[idx+1:] == ruleSink {
			return true
		}
	}
	return false
}

// AssertAtMostOnceEmit verifies that no key extracted via jsonPathKey is emitted
// more than once to sink across the current session (including across retries).
// It is used by kavach.Step.AssertAtMostOnceEmit.
func (e *Evaluator) AssertAtMostOnceEmit(sink, jsonPathKey string, s StepSnapshot) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	bucketKey := "code:" + sink + ":" + jsonPathKey
	return e.checkAtMostOnceLocked(bucketKey, sink, jsonPathKey, nil, s)
}

// CheckStep evaluates all declarative rules against step s.
// For failing steps (s.Err != nil || s.Panicked), it still records emitted keys
// for at_most_once_emit (so a subsequent retry catches duplicates) and enforces
// any explicit on_error_emit_count limit.
func (e *Evaluator) CheckStep(s StepSnapshot) *Violation {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, out := range s.Outputs {
		e.emittedBySink[out.Sink]++
	}

	failed := s.Err != nil || s.Panicked

	for _, r := range e.rules {
		switch r.Kind {
		case KindAtMostOnceEmit:
			if err := e.checkAtMostOnceLocked("rule:"+r.Name, r.Sink, r.Key, r.OnErrorEmitCount, s); err != nil {
				return &Violation{Name: r.Name, Kind: r.Kind, Detail: err.Error()}
			}
		case KindAntiLobotomy:
			if failed {
				continue
			}
			if v := e.checkAntiLobotomyStepLocked(r, s); v != nil {
				return v
			}
		case KindNonNegative:
			if failed {
				continue
			}
			if v := checkNonNegative(r, s); v != nil {
				return v
			}
		case KindConservation:
			if failed {
				continue
			}
			if v := checkConservation(r, s); v != nil {
				return v
			}
		}
	}
	return nil
}

// CheckSession evaluates whole-session rules such as aggregate anti_lobotomy
// once baseline counts are configured.
func (e *Evaluator) CheckSession() *Violation {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, r := range e.rules {
		if r.Kind != KindAntiLobotomy {
			continue
		}
		minRatio := 1.0
		if r.MinRatioVsBaseline != nil {
			minRatio = *r.MinRatioVsBaseline
		}
		sinks := r.Sinks
		if len(sinks) == 0 && r.Sink != "" {
			sinks = []string{r.Sink}
		}
		for _, wantSink := range sinks {
			baseCount := countMatchingSinks(e.baselineBySink, wantSink)
			if baseCount <= 0 {
				continue
			}
			gotCount := countMatchingSinks(e.emittedBySink, wantSink)
			ratio := float64(gotCount) / float64(baseCount)
			if ratio+1e-9 < minRatio {
				return &Violation{
					Name: r.Name,
					Kind: r.Kind,
					Detail: fmt.Sprintf("sink %q emitted %d output(s) vs baseline %d (ratio %.2f < min %.2f)",
						wantSink, gotCount, baseCount, ratio, minRatio),
				}
			}
		}
	}
	return nil
}

func (e *Evaluator) checkAtMostOnceLocked(bucketKey, sink, keyExpr string, onErrorLimit *int, s StepSnapshot) error {
	seen := e.seenByRule[bucketKey]
	if seen == nil {
		seen = make(map[string]uint64)
		e.seenByRule[bucketKey] = seen
	}

	matchingCount := 0
	var dupErr error
	for _, out := range s.Outputs {
		if !SinkMatches(out.Sink, sink) {
			continue
		}
		matchingCount++
		k := extractOutputKey(out, keyExpr)
		if k == "" {
			continue
		}
		if prevSeq, exists := seen[k]; exists {
			if dupErr == nil {
				dupErr = fmt.Errorf("duplicate emit to sink %q for key %q=%q (already emitted at step %d)",
					out.Sink, keyExpr, k, prevSeq)
			}
		} else {
			seen[k] = s.Seq
		}
	}

	if (s.Err != nil || s.Panicked) && onErrorLimit != nil && matchingCount > *onErrorLimit {
		return fmt.Errorf("emitted %d output(s) to sink %q during failing step %d (on_error_emit_count limit is %d)",
			matchingCount, sink, s.Seq, *onErrorLimit)
	}
	return dupErr
}

func (e *Evaluator) checkAntiLobotomyStepLocked(r Rule, s StepSnapshot) *Violation {
	if len(s.BaselineOutputs) == 0 {
		return nil
	}
	minRatio := 1.0
	if r.MinRatioVsBaseline != nil {
		minRatio = *r.MinRatioVsBaseline
	}
	sinks := r.Sinks
	if len(sinks) == 0 && r.Sink != "" {
		sinks = []string{r.Sink}
	}
	for _, wantSink := range sinks {
		base := 0
		for _, o := range s.BaselineOutputs {
			if SinkMatches(o.Sink, wantSink) {
				base++
			}
		}
		if base == 0 {
			continue
		}
		got := 0
		for _, o := range s.Outputs {
			if SinkMatches(o.Sink, wantSink) {
				got++
			}
		}
		ratio := float64(got) / float64(base)
		if ratio+1e-9 < minRatio {
			return &Violation{
				Name: r.Name,
				Kind: r.Kind,
				Detail: fmt.Sprintf("step %d sink %q emitted %d output(s) vs baseline %d (ratio %.2f < min %.2f)",
					s.Seq, wantSink, got, base, ratio, minRatio),
			}
		}
	}
	return nil
}

func checkNonNegative(r Rule, s StepSnapshot) *Violation {
	field := r.Field
	if field == "" {
		field = r.Key
	}
	if field == "" {
		return nil
	}
	for _, out := range s.Outputs {
		if !SinkMatches(out.Sink, r.Sink) {
			continue
		}
		vals, ok := ExtractJSONPathFloats(out.Data, field)
		if !ok {
			continue
		}
		for _, v := range vals {
			if v < 0 {
				return &Violation{
					Name:   r.Name,
					Kind:   r.Kind,
					Detail: fmt.Sprintf("sink %q field %q = %g < 0", out.Sink, field, v),
				}
			}
		}
	}
	return nil
}

func checkConservation(r Rule, s StepSnapshot) *Violation {
	field := r.Field
	if field == "" {
		field = r.Key
	}
	if field == "" {
		return nil
	}
	sum := 0.0
	foundAny := false
	for _, out := range s.Outputs {
		if !SinkMatches(out.Sink, r.Sink) {
			continue
		}
		vals, ok := ExtractJSONPathFloats(out.Data, field)
		if !ok {
			continue
		}
		for _, v := range vals {
			sum += v
			foundAny = true
		}
	}
	if foundAny && math.Abs(sum) > 1e-9 {
		return &Violation{
			Name:   r.Name,
			Kind:   r.Kind,
			Detail: fmt.Sprintf("step %d sink %q field %q sum = %g != 0", s.Seq, r.Sink, field, sum),
		}
	}
	return nil
}

func extractOutputKey(out Output, keyExpr string) string {
	if keyExpr != "" {
		if v, ok := ExtractJSONPathString(out.Data, keyExpr); ok && v != "" {
			return v
		}
	}
	if out.Key != "" {
		return out.Key
	}
	return ""
}

func countMatchingSinks(counts map[string]int, wantSink string) int {
	total := 0
	for s, n := range counts {
		if SinkMatches(s, wantSink) {
			total += n
		}
	}
	return total
}

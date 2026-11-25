package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/kavachlabs/kavach/internal/kconfig"
	"github.com/kavachlabs/kavach/journal"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// resolveInvariantRules loads rules from path (if non-empty) and appends any
// inline rules, validating all of them.
func resolveInvariantRules(path string, inline []kconfig.InvariantRule) ([]kconfig.InvariantRule, error) {
	var out []kconfig.InvariantRule
	if path != "" {
		f, err := kconfig.LoadInvariants(path)
		if err != nil {
			return nil, fmt.Errorf("kavach: loading invariants %s: %w", path, err)
		}
		out = append(out, f.Invariants...)
	}
	for i, r := range inline {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("kavach: inline invariant[%d]: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// recordBaselineOutputs counts recorded outputs per sink across steps.
func recordBaselineOutputs(steps []*step) map[string]int {
	counts := map[string]int{}
	for _, st := range steps {
		for _, o := range st.outputs {
			counts[o.Sink]++
		}
	}
	return counts
}

// resultBaselineOutputs counts emitted outputs per sink across a replay Result.
func resultBaselineOutputs(res *Result) map[string]int {
	counts := map[string]int{}
	if res == nil {
		return counts
	}
	for _, st := range res.Steps {
		for _, o := range st.Outputs {
			counts[o.Sink]++
		}
	}
	return counts
}

type invViolation struct {
	name   string
	seq    uint64
	detail string
}

type declarativeEvaluator struct {
	rules    []kconfig.InvariantRule
	baseline map[string]int

	// Per-rule state indexed by rule position.
	seenEmits  []map[string]uint64 // for at_most_once_emit: (sink + "\x00" + key) -> first seq
	groupSums  []map[string]float64
	groupSeqs  []map[string]uint64
	groupCount []map[string]int
	sinkCounts map[string]int
	lastSeq    uint64
}

func newDeclarativeEvaluator(rules []kconfig.InvariantRule, baseline map[string]int) *declarativeEvaluator {
	if len(rules) == 0 {
		return nil
	}
	ev := &declarativeEvaluator{
		rules:      rules,
		baseline:   baseline,
		seenEmits:  make([]map[string]uint64, len(rules)),
		groupSums:  make([]map[string]float64, len(rules)),
		groupSeqs:  make([]map[string]uint64, len(rules)),
		groupCount: make([]map[string]int, len(rules)),
		sinkCounts: map[string]int{},
	}
	for i, r := range rules {
		switch r.Kind {
		case kconfig.KindAtMostOnceEmit:
			ev.seenEmits[i] = map[string]uint64{}
		case kconfig.KindConservation:
			if r.Key != "" {
				ev.groupSums[i] = map[string]float64{}
				ev.groupSeqs[i] = map[string]uint64{}
				ev.groupCount[i] = map[string]int{}
			}
		}
	}
	return ev
}

// checkStep evaluates all per-step declarative invariants on sr (including
// failed attempts so on_error_emit_count and leaked emits are caught).
func (ev *declarativeEvaluator) checkStep(sr StepResult) *invViolation {
	if ev == nil {
		return nil
	}
	ev.lastSeq = sr.Seq
	for _, o := range sr.Outputs {
		ev.sinkCounts[o.Sink]++
	}
	stepFailed := sr.Error != "" || sr.Panic != "" || sr.Crash != "" || sr.Timeout != ""

	for i, r := range ev.rules {
		switch r.Kind {
		case kconfig.KindAtMostOnceEmit:
			if v := ev.checkAtMostOnce(i, r, sr, stepFailed); v != nil {
				return v
			}
		case kconfig.KindNonNegative:
			if v := ev.checkNonNegative(r, sr); v != nil {
				return v
			}
		case kconfig.KindConservation:
			if v := ev.checkConservationStep(i, r, sr, stepFailed); v != nil {
				return v
			}
		}
	}
	return nil
}

// checkEnd evaluates end-of-run declarative invariants (anti_lobotomy and
// cross-step keyed conservation).
func (ev *declarativeEvaluator) checkEnd() *invViolation {
	if ev == nil {
		return nil
	}
	for i, r := range ev.rules {
		switch r.Kind {
		case kconfig.KindAntiLobotomy:
			if v := ev.checkAntiLobotomy(r); v != nil {
				return v
			}
		case kconfig.KindConservation:
			if r.Key != "" {
				if v := ev.checkConservationEnd(i, r); v != nil {
					return v
				}
			}
		}
	}
	return nil
}

func (ev *declarativeEvaluator) checkAtMostOnce(idx int, r kconfig.InvariantRule, sr StepResult, stepFailed bool) *invViolation {
	matched := 0
	for _, o := range sr.Outputs {
		if ruleMatchesSink(r, o.Sink) {
			matched++
		}
	}
	if stepFailed && r.OnErrorEmitCount != nil && matched > *r.OnErrorEmitCount {
		return &invViolation{
			name: r.Name,
			seq:  sr.Seq,
			detail: fmt.Sprintf("step seq %d failed (%s) but emitted %d output(s) to %s (on_error_emit_count limit is %d)",
				sr.Seq, failure(sr), matched, sinkLabel(r), *r.OnErrorEmitCount),
		}
	}
	seen := ev.seenEmits[idx]
	for _, o := range sr.Outputs {
		if !ruleMatchesSink(r, o.Sink) {
			continue
		}
		if r.Key != "" {
			vals := extractJSONPathValues(o.Data, r.Key)
			for _, val := range vals {
				ks := formatJSONKey(val)
				if ks == "" {
					continue
				}
				ck := o.Sink + "\x00" + ks
				if prev, dup := seen[ck]; dup {
					return &invViolation{
						name: r.Name,
						seq:  sr.Seq,
						detail: fmt.Sprintf("duplicate emit on sink %q for key %s=%s (first emitted at step seq %d, again at step seq %d)",
							o.Sink, r.Key, ks, prev, sr.Seq),
					}
				}
				seen[ck] = sr.Seq
			}
		} else {
			ck := o.Sink + "\x00" + string(o.Data)
			if prev, dup := seen[ck]; dup {
				return &invViolation{
					name: r.Name,
					seq:  sr.Seq,
					detail: fmt.Sprintf("duplicate emit on sink %q (first emitted at step seq %d, again at step seq %d)",
						o.Sink, prev, sr.Seq),
				}
			}
			seen[ck] = sr.Seq
		}
	}
	return nil
}

func (ev *declarativeEvaluator) checkNonNegative(r kconfig.InvariantRule, sr StepResult) *invViolation {
	field := r.Field
	if field == "" {
		return nil
	}
	for _, o := range sr.Outputs {
		if !ruleMatchesSink(r, o.Sink) {
			continue
		}
		for _, val := range extractJSONPathValues(o.Data, field) {
			num, ok := toFloat64(val)
			if !ok {
				continue
			}
			if num < 0 {
				return &invViolation{
					name: r.Name,
					seq:  sr.Seq,
					detail: fmt.Sprintf("sink %q output field %s = %s is negative at step seq %d",
						o.Sink, field, formatJSONKey(val), sr.Seq),
				}
			}
		}
	}
	return nil
}

func (ev *declarativeEvaluator) checkConservationStep(idx int, r kconfig.InvariantRule, sr StepResult, stepFailed bool) *invViolation {
	if r.Field == "" {
		return nil
	}
	if r.Key != "" {
		// Accumulate per-key group deltas; validated at checkEnd.
		for _, o := range sr.Outputs {
			if !ruleMatchesSink(r, o.Sink) {
				continue
			}
			keyVals := extractJSONPathValues(o.Data, r.Key)
			if len(keyVals) == 0 {
				continue
			}
			groupKey := formatJSONKey(keyVals[0])
			for _, dv := range extractJSONPathValues(o.Data, r.Field) {
				num, ok := toFloat64(dv)
				if !ok {
					continue
				}
				ev.groupSums[idx][groupKey] += num
				ev.groupSeqs[idx][groupKey] = sr.Seq
				ev.groupCount[idx][groupKey]++
			}
		}
		return nil
	}
	if stepFailed {
		return nil
	}
	var sum float64
	count := 0
	for _, o := range sr.Outputs {
		if !ruleMatchesSink(r, o.Sink) {
			continue
		}
		for _, dv := range extractJSONPathValues(o.Data, r.Field) {
			num, ok := toFloat64(dv)
			if !ok {
				continue
			}
			sum += num
			count++
		}
	}
	if count > 0 && math.Abs(sum) > 1e-9 {
		return &invViolation{
			name: r.Name,
			seq:  sr.Seq,
			detail: fmt.Sprintf("conservation violated on %s field %s: sum of %d delta(s) = %g (want 0) at step seq %d",
				sinkLabel(r), r.Field, count, sum, sr.Seq),
		}
	}
	return nil
}

func (ev *declarativeEvaluator) checkConservationEnd(idx int, r kconfig.InvariantRule) *invViolation {
	// Check deterministically by smallest seq first.
	var best *invViolation
	for gk, sum := range ev.groupSums[idx] {
		if ev.groupCount[idx][gk] == 0 {
			continue
		}
		if math.Abs(sum) > 1e-9 {
			seq := ev.groupSeqs[idx][gk]
			v := &invViolation{
				name: r.Name,
				seq:  seq,
				detail: fmt.Sprintf("conservation violated on %s for %s=%s field %s: sum of %d delta(s) = %g (want 0)",
					sinkLabel(r), r.Key, gk, r.Field, ev.groupCount[idx][gk], sum),
			}
			if best == nil || v.seq < best.seq || (v.seq == best.seq && v.detail < best.detail) {
				best = v
			}
		}
	}
	return best
}

func (ev *declarativeEvaluator) checkAntiLobotomy(r kconfig.InvariantRule) *invViolation {
	if len(ev.baseline) == 0 {
		return nil
	}
	minRatio := r.MinRatioVsBaseline
	if minRatio == 0 {
		minRatio = 1.0
	}
	targets := r.TargetSinks()
	if len(targets) > 0 {
		for _, target := range targets {
			baseCount := countMatchingSink(ev.baseline, target)
			if baseCount == 0 {
				continue
			}
			gotCount := countMatchingSink(ev.sinkCounts, target)
			ratio := float64(gotCount) / float64(baseCount)
			if ratio+1e-9 < minRatio {
				return &invViolation{
					name: r.Name,
					seq:  ev.lastSeq,
					detail: fmt.Sprintf("sink %q emitted %d output(s) vs baseline %d (ratio %.3f < min_ratio_vs_baseline %.3f)",
						target, gotCount, baseCount, ratio, minRatio),
				}
			}
		}
		return nil
	}
	totalBase, totalGot := 0, 0
	for _, c := range ev.baseline {
		totalBase += c
	}
	for _, c := range ev.sinkCounts {
		totalGot += c
	}
	if totalBase > 0 {
		ratio := float64(totalGot) / float64(totalBase)
		if ratio+1e-9 < minRatio {
			return &invViolation{
				name: r.Name,
				seq:  ev.lastSeq,
				detail: fmt.Sprintf("emitted %d total output(s) vs baseline %d (ratio %.3f < min_ratio_vs_baseline %.3f)",
					totalGot, totalBase, ratio, minRatio),
			}
		}
	}
	return nil
}

// applyDeclarativeInvariants evaluates rules post-hoc against a Result (used by
// VerifyWith when ReplayFunc was not given Options.Invariants directly, or to
// enforce anti_lobotomy with per-variant baseline counts). It is idempotent if
// res already failed on an invariant or earlier error.
func applyDeclarativeInvariants(res *Result, rules []kconfig.InvariantRule, baseline map[string]int) {
	if res == nil || len(rules) == 0 {
		return
	}
	res.Invariants = appendUniqueInvariantNames(res.Invariants, rules)
	if res.Status == StatusInvariantViolated || res.Status == StatusNondeterministic {
		return
	}
	ev := newDeclarativeEvaluator(rules, baseline)
	for i, sr := range res.Steps {
		if v := ev.checkStep(sr); v != nil {
			res.Steps = res.Steps[:i+1]
			res.Invariant = v.name
			res.set(StatusInvariantViolated, v.seq, v.detail)
			return
		}
	}
	if !res.Passed() {
		return
	}
	if v := ev.checkEnd(); v != nil {
		res.Invariant = v.name
		res.set(StatusInvariantViolated, v.seq, v.detail)
	}
}

func appendUniqueInvariantNames(existing []string, rules []kconfig.InvariantRule) []string {
	seen := make(map[string]bool, len(existing))
	for _, n := range existing {
		seen[n] = true
	}
	for _, r := range rules {
		if r.Name != "" && !seen[r.Name] {
			seen[r.Name] = true
			existing = append(existing, r.Name)
		}
	}
	return existing
}

func ruleMatchesSink(r kconfig.InvariantRule, sink string) bool {
	targets := r.TargetSinks()
	if len(targets) == 0 {
		return true
	}
	for _, t := range targets {
		if sinkMatchesTarget(sink, t) {
			return true
		}
	}
	return false
}

func sinkMatchesTarget(sink, target string) bool {
	if sink == target {
		return true
	}
	// If target has no driver prefix (e.g. "events_charged_in_advance"),
	// also match "<driver>:events_charged_in_advance".
	if !strings.Contains(target, ":") && strings.HasSuffix(sink, ":"+target) {
		return true
	}
	return false
}

func countMatchingSink(counts map[string]int, target string) int {
	total := 0
	for sink, n := range counts {
		if sinkMatchesTarget(sink, target) {
			total += n
		}
	}
	return total
}

func sinkLabel(r kconfig.InvariantRule) string {
	targets := r.TargetSinks()
	if len(targets) == 0 {
		return "any sink"
	}
	if len(targets) == 1 {
		return fmt.Sprintf("sink %q", targets[0])
	}
	return fmt.Sprintf("sinks %v", targets)
}

type jpStep struct {
	key      string
	isIndex  bool
	wildcard bool
	index    int
}

func parseJSONPath(path string) []jpStep {
	p := strings.TrimSpace(path)
	p = strings.TrimPrefix(p, "$")
	p = strings.TrimPrefix(p, ".")
	if p == "" {
		return nil
	}
	var steps []jpStep
	i := 0
	for i < len(p) {
		if p[i] == '.' {
			i++
			continue
		}
		if p[i] == '[' {
			end := strings.IndexByte(p[i:], ']')
			if end < 0 {
				break
			}
			inner := strings.TrimSpace(p[i+1 : i+end])
			if inner == "*" {
				steps = append(steps, jpStep{isIndex: true, wildcard: true})
			} else if idx, err := strconv.Atoi(inner); err == nil {
				steps = append(steps, jpStep{isIndex: true, index: idx})
			} else {
				inner = strings.Trim(inner, `"'`)
				steps = append(steps, jpStep{key: inner})
			}
			i += end + 1
			continue
		}
		j := i
		for j < len(p) && p[j] != '.' && p[j] != '[' {
			j++
		}
		if j > i {
			steps = append(steps, jpStep{key: p[i:j]})
		}
		i = j
	}
	return steps
}

// extractJSONPathValues extracts all values matching path from a JSON payload.
// Supports $.a.b, a.b, $.arr[0].x, and $.arr[*].x.
func extractJSONPathValues(data []byte, path string) []any {
	steps := parseJSONPath(path)
	if len(steps) == 0 || len(data) == 0 {
		return nil
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil
	}
	cur := []any{root}
	for _, st := range steps {
		var next []any
		for _, node := range cur {
			if st.isIndex {
				arr, ok := node.([]any)
				if !ok {
					continue
				}
				if st.wildcard {
					next = append(next, arr...)
				} else if st.index >= 0 && st.index < len(arr) {
					next = append(next, arr[st.index])
				}
			} else {
				m, ok := node.(map[string]any)
				if !ok {
					continue
				}
				if v, exists := m[st.key]; exists {
					next = append(next, v)
				}
			}
		}
		cur = next
		if len(cur) == 0 {
			return nil
		}
	}
	return cur
}

func formatJSONKey(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// baselineForJournal extracts recorded output counts per sink from j.
func baselineForJournal(j *journal.Journal) map[string]int {
	counts := map[string]int{}
	if j == nil {
		return counts
	}
	for _, r := range j.Records {
		if r.Type == journal.TypeOutput {
			counts[r.Sink]++
		}
	}
	return counts
}

// EvalInvariants evaluates declarative invariant rules against res and returns
// the updated Result (non-mutating copy if violated? No, evaluates in place
// matching applyDeclarativeInvariants). Exposed for callers inspecting Results.
func EvalInvariants(res *Result, rules []kconfig.InvariantRule, baseline map[string]int) {
	applyDeclarativeInvariants(res, rules, baseline)
}

// OutputSliceToMap is a small helper unused externally; keep kavach import used
// directly via StepResult.Outputs.
var _ = kavach.Output{}

package replay

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

const gatewayFaultMutationPrefix = "gateway fault"

// transientFaultForGateway returns the primary realistic transient error
// injected for a gateway call during driver-level fault & retry mutation.
func transientFaultForGateway(gateway string) string {
	switch driverOf(gateway) {
	case "redis":
		return "LOADING Redis is loading the dataset in memory"
	case "pg", "pgx", "postgres", "postgresql", "sql":
		return "ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)"
	default:
		return "context deadline exceeded"
	}
}

// alternateFaultsForGateway returns additional realistic transient errors for a
// driver so fault-injection schedules cover multiple failure modes.
func alternateFaultsForGateway(gateway string) []string {
	switch driverOf(gateway) {
	case "redis":
		return []string{
			"READONLY You can't write against a read only replica.",
			"CLUSTERDOWN Hash slot not served",
			"i/o timeout",
		}
	case "pg", "pgx", "postgres", "postgresql", "sql":
		return []string{
			"ERROR: deadlock detected (SQLSTATE 40P01)",
			"connection reset by peer",
			"context deadline exceeded",
		}
	default:
		return []string{
			"EOF",
			"503 Service Unavailable",
			"connection refused",
		}
	}
}

func hasGatewayRecords(j *journal.Journal) bool {
	if j == nil {
		return false
	}
	for _, r := range j.Records {
		if r.Type == journal.TypeGateway {
			return true
		}
	}
	return false
}

func isFaultInjectionVariant(j *journal.Journal) bool {
	if j == nil || j.Header.Meta.Variant == nil {
		return false
	}
	v := j.Header.Meta.Variant
	return v.Kind == KindGatewayFault || strings.HasPrefix(v.Mutation, gatewayFaultMutationPrefix)
}

// isRetryableFaultAttempt reports whether step idx inside a fault-injection
// variant journal is an error-injected attempt that is immediately followed by
// a redelivery of the identical input.
func isRetryableFaultAttempt(j *journal.Journal, steps []*step, idx int) bool {
	if !isFaultInjectionVariant(j) || idx < 0 || idx+1 >= len(steps) {
		return false
	}
	cur := steps[idx]
	next := steps[idx+1]
	if cur.input.Source != next.input.Source || !bytes.Equal(cur.input.Data, next.input.Data) {
		return false
	}
	for _, r := range cur.reads {
		if r.Type == journal.TypeGateway && r.Error != "" {
			return true
		}
	}
	return false
}

// faultAttemptStartSeq returns the seq of the first injected-fault attempt in
// a fault-injection variant journal, or (0, false) if none exists.
func faultAttemptStartSeq(j *journal.Journal) (uint64, bool) {
	if !isFaultInjectionVariant(j) {
		return 0, false
	}
	recs := j.Records
	if j.Header.Meta.Start == journal.StartSnapshot && len(recs) > 0 {
		recs = recs[1:]
	}
	steps, err := splitSteps(recs)
	if err != nil {
		return 0, false
	}
	for i := range steps {
		if isRetryableFaultAttempt(j, steps, i) {
			return steps[i].input.Seq, true
		}
	}
	return 0, false
}

type faultCandidate struct {
	cand     candidate
	baseline map[string]int
}

type variantBundle struct {
	journal  *journal.Journal
	baseline map[string]int
}

func cloneReadsWithFault(reads []journal.Record, readIdx int, errMsg string) []journal.Record {
	out := make([]journal.Record, len(reads))
	for i, r := range reads {
		out[i] = r.Clone()
	}
	out[readIdx].Response = nil
	out[readIdx].Error = errMsg
	return out
}

func cloneReadsShiftClock(reads []journal.Record, delta time.Duration) []journal.Record {
	out := make([]journal.Record, len(reads))
	for i, r := range reads {
		rc := r.Clone()
		if rc.Type == journal.TypeClock {
			rc.UnixNanos += int64(delta)
		}
		out[i] = rc
	}
	return out
}

// gatewayFaults generates driver-level fault-injection + redelivery candidates
// for step k inside base[0..k] (used by Variants on incident journals that
// contain Gateway calls). Returns nil if base[k] has no healthy Gateway reads.
func gatewayFaults(base []vstep, origSteps []*step, k int) []candidate {
	raw := faultCandidatesForStep(base, origSteps, k, false)
	if len(raw) == 0 {
		return nil
	}
	out := make([]candidate, len(raw))
	for i, fc := range raw {
		out[i] = fc.cand
	}
	return out
}

// BaselineFaultVariants generates driver-level fault-injection + redelivery
// variants directly from a healthy (or recorded) capsule containing Gateway
// calls. Each variant injects a transient error on the k-th gateway call of a
// step and immediately redelivers the identical input with the original
// recorded gateway responses.
func BaselineFaultVariants(j *journal.Journal, limit int) ([]*journal.Journal, error) {
	bundles, err := baselineFaultBundles(j, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*journal.Journal, len(bundles))
	for i, b := range bundles {
		out[i] = b.journal
	}
	return out, nil
}

func baselineFaultBundles(j *journal.Journal, limit int) ([]variantBundle, error) {
	if limit <= 0 {
		limit = MaxCandidates
	}
	snap, env, steps, err := variantSource(j)
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, nil
	}
	base := make([]vstep, len(steps))
	for i, st := range steps {
		base[i] = vstep{input: st.input, reads: st.reads}
	}

	// Build fault candidate streams per step (interleaved across steps so every
	// step and driver gets covered early).
	var perStep [][]faultCandidate
	for sIdx := len(steps) - 1; sIdx >= 0; sIdx-- {
		cs := faultCandidatesForStep(base, steps, sIdx, true)
		if len(cs) > 0 {
			perStep = append(perStep, cs)
		}
	}
	if len(perStep) == 0 {
		return nil, nil
	}

	var out []variantBundle
	for round := 0; len(out) < limit; round++ {
		added := false
		for _, cs := range perStep {
			if round < len(cs) && len(out) < limit {
				fc := cs[round]
				info := journal.Variant{
					ID:       len(out) + 1,
					Mutation: fc.cand.mutation,
					Kind:     KindGatewayFault,
				}
				vj := buildVariant(j.Header.Meta, snap, env, fc.cand.steps, info)
				out = append(out, variantBundle{journal: vj, baseline: fc.baseline})
				added = true
			}
		}
		if !added {
			break
		}
	}
	return out, nil
}

// faultCandidatesForStep generates deterministic, interleaved driver-level
// fault-injection + redelivery schedules for step sIdx.
// When keepTail is true (healthy baseline capsules), steps after sIdx are
// preserved so full-trace invariants (like anti_lobotomy) remain accurate.
func faultCandidatesForStep(base []vstep, origSteps []*step, sIdx int, keepTail bool) []faultCandidate {
	st := base[sIdx]
	type gwSlot struct {
		readIdx       int
		callNum       int // 1-based gateway call index within step
		gateway       string
		outputsBefore int
	}
	var outSeqs []uint64
	if sIdx < len(origSteps) && origSteps[sIdx] != nil {
		outSeqs = origSteps[sIdx].outputSeqs
	}
	var slots []gwSlot
	gwNum := 0
	for i, r := range st.reads {
		if r.Type == journal.TypeGateway {
			gwNum++
			if r.Error == "" {
				ob := 0
				for _, oseq := range outSeqs {
					if oseq < r.Seq {
						ob++
					}
				}
				slots = append(slots, gwSlot{readIdx: i, callNum: gwNum, gateway: r.Gateway, outputsBefore: ob})
			}
		}
	}
	if len(slots) == 0 {
		return nil
	}
	// Order slots from LAST gateway call to FIRST (and highest outputsBefore first)
	// so the earliest variants hit the deepest point in the handler where side
	// effects have already been emitted (e.g. Lago L1's trailing Redis ZADD).
	for l, r := 0, len(slots)-1; l < r; l, r = l+1, r-1 {
		slots[l], slots[r] = slots[r], slots[l]
	}
	// Separate post-emit slots (gateways issued after >= 1 output in the recorded
	// step). Group 1 always probes ALL slots; deeper mutation groups focus on
	// post-emit slots when any exist so high-risk retry-leak paths get full
	// variant budget coverage (e.g. --variants 25 on multi-query handlers).
	var postEmitSlots []gwSlot
	for _, sl := range slots {
		if sl.outputsBefore > 0 {
			postEmitSlots = append(postEmitSlots, sl)
		}
	}
	deepSlots := slots
	if len(postEmitSlots) > 0 {
		deepSlots = postEmitSlots
	}

	fullBaseline := computeStepBaseline(origSteps, 0, len(base), -1)
	if !keepTail {
		fullBaseline = computeStepBaseline(origSteps, 0, sIdx+1, -1)
	}

	makeSteps := func(prefix []vstep, attempts int, faultReadIdx int, errMsg string, inputOverride []byte, clockDelta time.Duration, tail []vstep) []vstep {
		out := make([]vstep, 0, len(prefix)+attempts+1+len(tail))
		for _, p := range prefix {
			cp := p
			cp.incident = false
			if clockDelta != 0 {
				cp.reads = cloneReadsShiftClock(p.reads, clockDelta)
			}
			out = append(out, cp)
		}
		targetIn := st.input.Clone()
		if inputOverride != nil {
			targetIn.Data = append([]byte(nil), inputOverride...)
		}
		for a := 0; a < attempts; a++ {
			reads := cloneReadsWithFault(st.reads, faultReadIdx, errMsg)
			if clockDelta != 0 || a > 0 {
				reads = cloneReadsShiftClock(reads, clockDelta+time.Duration(a)*100*time.Millisecond)
			}
			out = append(out, vstep{input: targetIn.Clone(), reads: reads, incident: false})
		}
		recReads := st.reads
		if clockDelta != 0 || attempts > 0 {
			recReads = cloneReadsShiftClock(st.reads, clockDelta+time.Duration(attempts)*100*time.Millisecond)
		}
		out = append(out, vstep{input: targetIn.Clone(), reads: recReads, incident: true})
		for _, t := range tail {
			ct := t
			ct.incident = false
			if clockDelta != 0 {
				ct.reads = cloneReadsShiftClock(t.reads, clockDelta)
			}
			out = append(out, ct)
		}
		return out
	}

	var tail []vstep
	if keepTail && sIdx+1 < len(base) {
		tail = base[sIdx+1:]
	}
	prefix := base[:sIdx]

	var groups [][]faultCandidate

	// Group 1: Primary transient fault + single redelivery (1 per gateway slot).
	var gPrimary []faultCandidate
	for _, sl := range slots {
		errMsg := transientFaultForGateway(sl.gateway)
		desc := fmt.Sprintf("%s on step seq %d (%s #%d): %s + redelivery",
			gatewayFaultMutationPrefix, st.input.Seq, sl.gateway, sl.callNum, errMsg)
		gPrimary = append(gPrimary, faultCandidate{
			cand:     candidate{mutation: desc, steps: makeSteps(prefix, 1, sl.readIdx, errMsg, nil, 0, tail)},
			baseline: fullBaseline,
		})
	}
	groups = append(groups, gPrimary)

	// Group 2: Double transient fault (2 failed attempts + 3rd recovery attempt).
	var gDouble []faultCandidate
	for _, sl := range deepSlots {
		errMsg := transientFaultForGateway(sl.gateway)
		desc := fmt.Sprintf("%s on step seq %d (%s #%d): 2x %s + redelivery",
			gatewayFaultMutationPrefix, st.input.Seq, sl.gateway, sl.callNum, errMsg)
		gDouble = append(gDouble, faultCandidate{
			cand:     candidate{mutation: desc, steps: makeSteps(prefix, 2, sl.readIdx, errMsg, nil, 0, tail)},
			baseline: fullBaseline,
		})
	}
	groups = append(groups, gDouble)

	// Group 3: Alternate transient driver faults + redelivery.
	var gAlt []faultCandidate
	for altIdx := 0; altIdx < 3; altIdx++ {
		for _, sl := range deepSlots {
			alts := alternateFaultsForGateway(sl.gateway)
			if altIdx >= len(alts) {
				continue
			}
			errMsg := alts[altIdx]
			desc := fmt.Sprintf("%s on step seq %d (%s #%d): %s + redelivery",
				gatewayFaultMutationPrefix, st.input.Seq, sl.gateway, sl.callNum, errMsg)
			gAlt = append(gAlt, faultCandidate{
				cand:     candidate{mutation: desc, steps: makeSteps(prefix, 1, sl.readIdx, errMsg, nil, 0, tail)},
				baseline: fullBaseline,
			})
		}
	}
	if len(gAlt) > 0 {
		groups = append(groups, gAlt)
	}

	// Group 4: Primary transient fault + clock shifts (only when clock reads exist).
	if hasAnyClockReads(base) {
		faultClockShifts := []struct {
			d     time.Duration
			label string
		}{
			{time.Millisecond, "+1ms"},
			{time.Second, "+1s"},
			{time.Minute, "+1m"},
			{time.Hour, "+1h"},
			{90 * time.Minute, "+1h30m"},
			{-5*time.Hour - 30*time.Minute, "-5h30m"},
		}
		var gClock []faultCandidate
		for _, sh := range faultClockShifts {
			for _, sl := range deepSlots {
				errMsg := transientFaultForGateway(sl.gateway)
				desc := fmt.Sprintf("%s on step seq %d (%s #%d): %s + redelivery (clock %s)",
					gatewayFaultMutationPrefix, st.input.Seq, sl.gateway, sl.callNum, errMsg, sh.label)
				gClock = append(gClock, faultCandidate{
					cand:     candidate{mutation: desc, steps: makeSteps(prefix, 1, sl.readIdx, errMsg, nil, sh.d, tail)},
					baseline: fullBaseline,
				})
			}
		}
		if len(gClock) > 0 {
			groups = append(groups, gClock)
		}
	}

	// Group 5: Primary transient fault + history drop / prior redelivery (when sIdx > 0).
	if sIdx > 0 {
		var gHist []faultCandidate
		for d := sIdx - 1; d >= 0; d-- {
			droppedPrefix := append(cloneSteps(prefix[:d]), prefix[d+1:]...)
			droppedBase := computeStepBaseline(origSteps, 0, len(base), d)
			if !keepTail {
				droppedBase = computeStepBaseline(origSteps, 0, sIdx+1, d)
			}
			for _, sl := range deepSlots {
				errMsg := transientFaultForGateway(sl.gateway)
				desc := fmt.Sprintf("%s on step seq %d (%s #%d): %s + redelivery (input seq %d dropped)",
					gatewayFaultMutationPrefix, st.input.Seq, sl.gateway, sl.callNum, errMsg, prefix[d].input.Seq)
				gHist = append(gHist, faultCandidate{
					cand:     candidate{mutation: desc, steps: makeSteps(droppedPrefix, 1, sl.readIdx, errMsg, nil, 0, tail)},
					baseline: droppedBase,
				})
			}
		}
		if len(gHist) > 0 {
			groups = append(groups, gHist)
		}
	}

	// Group 6: Primary transient fault + input field perturbations (same mutated
	// input delivered on both Attempt 1 and Attempt 2). Skip mutations that
	// copy a unique-per-step identifier/key from another step (which would
	// create an artificial cross-step duplicate key collision under
	// at_most_once_emit).
	fieldCands := fieldMutationsWith(base[:sIdx+1], sIdx, mutationOpts{signPreserving: true})
	if len(fieldCands) > 0 {
		var gFields []faultCandidate
		added := 0
		const maxFieldMuts = 20
		for _, fc := range fieldCands {
			if added >= maxFieldMuts {
				break
			}
			// Skip field removals on fault-injection variants so required schema
			// keys aren't stripped before the handler even reaches the gateway.
			if strings.HasSuffix(fc.mutation, " removed") {
				continue
			}
			mutData := fc.steps[sIdx].input.Data
			if introducesCrossStepKeyCollision(base, sIdx, mutData) {
				continue
			}
			added++
			mutLabel := fc.mutation
			if idx := strings.Index(mutLabel, ": "); idx >= 0 {
				mutLabel = mutLabel[idx+2:]
			}
			for _, sl := range deepSlots {
				errMsg := transientFaultForGateway(sl.gateway)
				desc := fmt.Sprintf("%s on step seq %d (%s #%d): %s + redelivery (%s)",
					gatewayFaultMutationPrefix, st.input.Seq, sl.gateway, sl.callNum, errMsg, mutLabel)
				gFields = append(gFields, faultCandidate{
					cand:     candidate{mutation: desc, steps: makeSteps(prefix, 1, sl.readIdx, errMsg, mutData, 0, tail)},
					baseline: fullBaseline,
				})
			}
		}
		if len(gFields) > 0 {
			groups = append(groups, gFields)
		}
	}

	// Interleave across groups so early candidates test diverse fault classes
	// (primary, double-retry, alt faults, clock shifts, history drops, field mutations).
	var out []faultCandidate
	for i := 0; ; i++ {
		more := false
		for _, g := range groups {
			if i < len(g) {
				out = append(out, g[i])
				more = true
			}
		}
		if !more {
			break
		}
	}
	return out
}

func hasAnyClockReads(steps []vstep) bool {
	for _, st := range steps {
		if hasClockReads(st.reads) {
			return true
		}
	}
	return false
}

func computeStepBaseline(steps []*step, start, end, skipIdx int) map[string]int {
	if len(steps) == 0 {
		return nil
	}
	counts := map[string]int{}
	if end > len(steps) {
		end = len(steps)
	}
	for i := start; i < end; i++ {
		if i == skipIdx {
			continue
		}
		for _, o := range steps[i].outputs {
			counts[o.Sink]++
		}
	}
	return counts
}

// introducesCrossStepKeyCollision reports whether mutData replaces a
// unique-per-step leaf value at sIdx with a value already used at the same
// JSON path by a different step in base. That prevents fault-injection input
// perturbations from synthesizing fake duplicate transaction/event IDs across
// unrelated steps.
func introducesCrossStepKeyCollision(base []vstep, sIdx int, mutData []byte) bool {
	if len(base) <= 1 {
		return false
	}
	mutLeaves := leaves(mutData)
	if len(mutLeaves) == 0 {
		return false
	}
	origLeaves := leaves(base[sIdx].input.Data)
	for path, newVal := range mutLeaves {
		if origLeaves[path] == newVal {
			continue
		}
		// Only guard string leaves (IDs, keys, tokens) that were distinct
		// across all steps of base before mutation.
		if len(newVal) == 0 || newVal[0] != '"' {
			continue
		}
		seen := map[string]bool{}
		uniqueInBase := true
		collidesOther := false
		for i, st := range base {
			v, ok := leaves(st.input.Data)[path]
			if !ok {
				continue
			}
			if seen[v] {
				uniqueInBase = false
				break
			}
			seen[v] = true
			if i != sIdx && v == newVal {
				collidesOther = true
			}
		}
		if uniqueInBase && collidesOther {
			return true
		}
	}
	return false
}

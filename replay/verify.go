package replay

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/kavachlabs/kavach/internal/kconfig"
	"github.com/kavachlabs/kavach/journal"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

const (
	// StatusVariantFailed: the fix passes the recorded incident, but the new
	// build fails a variant of it that the old build failed in the same way.
	StatusVariantFailed Status = "variant_failed"
	// StatusRegressed: the fix passes the recorded incident and its variants,
	// but the new build handles a variant of an earlier, similar input
	// differently from the old build, which handled it without failing.
	StatusRegressed Status = "regressed"
	// StatusUnverified: the fix passes the recorded incident, but too few
	// variants reproduce the incident on the old build to check it further.
	StatusUnverified Status = "unverified"
)

// DefaultMinVariants is the number of variants a fix must pass.
const DefaultMinVariants = 10

// DefaultRegressions is the number of regression variants generated.
const DefaultRegressions = MaxRegressionCandidates

// VerifyOptions configure Verify.
type VerifyOptions struct {
	// MinVariants is how many variants must reproduce the incident on the old
	// build, and then pass on the new one, for a fix to count. Zero means
	// DefaultMinVariants; a negative value skips variants.
	MinVariants int
	// MinKinds is the minimum number of distinct mutation kinds (field, pair,
	// move, drop, duplicate, clock) that must reproduce the incident on the
	// old build. Zero disables this check.
	MinKinds int
	// Parallel is how many variants are checked at once. Values below 1 mean
	// one. The result does not depend on it.
	Parallel int
	// Regressions is how many regression variants (see RegressionVariants) are
	// generated. Zero means DefaultRegressions; a negative value skips them.
	Regressions int
	// InvariantsPath, if non-empty, loads declarative invariants from a
	// .kavach/invariants.yaml file and evaluates them on both builds during
	// verification (including driver-level fault-injection variants).
	InvariantsPath string
	// Invariants are inline declarative invariant rules evaluated alongside
	// any InvariantsPath rules and handler-declared invariants.
	Invariants []kconfig.InvariantRule
}

// ReplayFunc replays a journal against one build of a handler.
type ReplayFunc func(j *journal.Journal) (*Result, error)

// VariantCheck is how one variant of the incident fared.
type VariantCheck struct {
	ID       int    `json:"id"`
	Mutation string `json:"mutation"`
	Kind     string `json:"kind,omitempty"`
	Incident uint64 `json:"incident_seq"`
	// Reproduces is true when the old build fails on the variant at
	// Incident as it failed in production. Only those variants count.
	Reproduces bool    `json:"reproduces"`
	Old        string  `json:"old"`
	New        string  `json:"new,omitempty"`
	Passed     bool    `json:"passed"`
	Seq        *uint64 `json:"seq,omitempty"`
	Detail     string  `json:"detail,omitempty"`
	// Journal is the variant itself.
	Journal *journal.Journal `json:"-"`
}

// RegressionCheck is how one variant of an earlier input that resembles the
// failing one fared.
type RegressionCheck struct {
	ID       int    `json:"id"`
	Mutation string `json:"mutation"`
	Input    uint64 `json:"input_seq"` // the perturbed earlier input, in the variant
	// Counted is true when the old build replays the variant as ok. Only those
	// variants say anything about the fix.
	Counted bool    `json:"counted"`
	Old     string  `json:"old"`
	New     string  `json:"new,omitempty"`
	Passed  bool    `json:"passed"`
	Seq     *uint64 `json:"seq,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Journal is the variant itself.
	Journal *journal.Journal `json:"-"`
}

// Verification is the outcome of checking a candidate fix against an incident.
type Verification struct {
	Status  Status  `json:"status"`
	Verdict string  `json:"verdict"`
	Variant int     `json:"variant,omitempty"`
	Seq     *uint64 `json:"seq,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Old and New are the two builds' replays of the recorded journal.
	Old *Result `json:"old"`
	New *Result `json:"new"`
	// MinVariants is the number of reproducing variants required.
	MinVariants       int            `json:"min_variants"`
	Candidates        int            `json:"candidates"`
	Reproducing       int            `json:"reproducing"`
	ReproducingByKind map[string]int `json:"reproducing_by_kind,omitempty"`
	Passing           int            `json:"passing"`
	Variants          []VariantCheck `json:"variants"`

	// Regression is the first regression variant the new build changed.
	Regression int `json:"regression,omitempty"`
	// Neighbours is the number of earlier inputs that resemble the failing one.
	Neighbours           int               `json:"neighbours"`
	RegressionCandidates int               `json:"regression_candidates"`
	RegressionCounted    int               `json:"regression_counted"`
	RegressionPassing    int               `json:"regression_passing"`
	Regressions          []RegressionCheck `json:"regressions"`
}

// String returns the verdict, e.g. "fixed", "still_failing@31" or
// "variant_failed(4)@12".
func (v *Verification) String() string { return v.Verdict }

// Passed reports whether the fix counts: the status is fixed or ok.
func (v *Verification) Passed() bool { return v.Status == StatusFixed || v.Status == StatusOK }

// Verify checks a candidate fix in process. See VerifyWith.
func Verify(j *journal.Journal, oldHandler, newHandler func() kavach.Handler, opts VerifyOptions) (*Verification, error) {
	ropts := Options{InvariantsPath: opts.InvariantsPath, Invariants: opts.Invariants}
	return VerifyWith(j,
		func(j *journal.Journal) (*Result, error) { return RunWith(j, oldHandler, ropts) },
		func(j *journal.Journal) (*Result, error) { return RunWith(j, newHandler, ropts) },
		opts)
}

// VerifyWith checks a candidate fix. A fix counts only if
//
//  1. the new build replays the recorded journal as fixed: the failure is
//     gone, declared invariants hold, and earlier steps produce the
//     recorded outputs;
//  2. at least MinVariants variants of the incident (see Variants) reproduce
//     it on the old build: the old build fails on the variant's failing input
//     exactly as it failed in production; and
//  3. the new build passes every such variant: no step fails, invariants
//     hold, and steps before the failing input produce the old build's
//     outputs; and
//  4. the new build replays every regression variant (see RegressionVariants)
//     that the old build replays as ok as ok too, with the old build's
//     outputs at every step.
//
// Variants on which the old build does not reproduce the incident say
// nothing about the fix and are ignored.
func VerifyWith(j *journal.Journal, replayOld, replayNew ReplayFunc, opts VerifyOptions) (*Verification, error) {
	min := opts.MinVariants
	if min == 0 {
		min = DefaultMinVariants
	}
	declRules, err := resolveInvariantRules(opts.InvariantsPath, opts.Invariants)
	if err != nil {
		return nil, err
	}
	recBaseline := baselineForJournal(j)
	runWithDecl := func(fn ReplayFunc, vj *journal.Journal, base map[string]int) (*Result, error) {
		res, err := fn(vj)
		if err != nil {
			return nil, err
		}
		applyDeclarativeInvariants(res, declRules, base)
		return res, nil
	}

	oldRes, err := runWithDecl(replayOld, j, recBaseline)
	if err != nil {
		return nil, fmt.Errorf("old build: %w", err)
	}
	newRes, err := runWithDecl(replayNew, j, recBaseline)
	if err != nil {
		return nil, fmt.Errorf("new build: %w", err)
	}
	v := &Verification{Old: oldRes, New: newRes, MinVariants: max(min, 0), Variants: []VariantCheck{}, Regressions: []RegressionCheck{}}
	if !newRes.Passed() || min < 0 {
		v.Status, v.Verdict, v.Seq, v.Detail = newRes.Status, newRes.String(), newRes.Seq, newRes.Detail
		return v, nil
	}
	if newRes.Status == StatusOK {
		if !hasGatewayRecords(j) {
			v.Status, v.Verdict, v.Seq, v.Detail = newRes.Status, newRes.String(), newRes.Seq, newRes.Detail
			return v, nil
		}
		return verifyHealthyBaselineFaults(j, v, replayOld, replayNew, opts, min, declRules)
	}

	variants, err := Variants(j)
	if err != nil {
		return nil, err
	}
	v.Candidates = len(variants)
	requireIncidentOutput := hasIncidentOutputs(oldRes)
	checks := make([]VariantCheck, len(variants))
	if err := parallel(len(variants), opts.Parallel, func(i int) (err error) {
		var base map[string]int
		if k := variants[i].Header.Meta.Variant.Kind; k != KindDrop && k != KindMove {
			base = recBaseline
		}
		checks[i], err = checkVariant(variants[i],
			func(vj *journal.Journal) (*Result, error) { return runWithDecl(replayOld, vj, base) },
			func(vj *journal.Journal) (*Result, error) { return runWithDecl(replayNew, vj, base) },
			requireIncidentOutput)
		return err
	}); err != nil {
		return nil, err
	}
	var byKind map[string]int
	for _, c := range checks {
		if c.Reproduces {
			v.Reproducing++
			if c.Kind != "" {
				if byKind == nil {
					byKind = map[string]int{}
				}
				byKind[c.Kind]++
			}
		}
		if c.Passed {
			v.Passing++
		} else if c.Reproduces && v.Variant == 0 {
			v.Variant, v.Seq = c.ID, c.Seq
		}
	}
	v.ReproducingByKind = byKind
	v.Variants = checks

	var regs []*journal.Journal
	var neighbours int
	if opts.Regressions >= 0 {
		if regs, neighbours, err = regressionVariants(j, opts.Regressions); err != nil {
			return nil, err
		}
	}
	v.RegressionCandidates = len(regs)
	rchecks := make([]RegressionCheck, len(regs))
	if err := parallel(len(regs), opts.Parallel, func(i int) (err error) {
		rchecks[i], err = checkRegression(regs[i],
			func(vj *journal.Journal) (*Result, error) { return runWithDecl(replayOld, vj, nil) },
			func(vj *journal.Journal) (*Result, error) { return runWithDecl(replayNew, vj, nil) })
		return err
	}); err != nil {
		return nil, err
	}
	for _, c := range rchecks {
		if c.Counted {
			v.RegressionCounted++
		}
		if c.Passed {
			v.RegressionPassing++
		} else if c.Counted && v.Regression == 0 {
			v.Regression = c.ID
		}
	}
	v.Regressions = rchecks
	v.Neighbours = neighbours

	switch {
	case v.Variant != 0:
		c := v.Variants[indexOf(v.Variants, v.Variant)]
		v.Status = StatusVariantFailed
		v.Verdict = fmt.Sprintf("%s(%d)", StatusVariantFailed, c.ID)
		if c.Seq != nil {
			v.Verdict += fmt.Sprintf("@%d", *c.Seq)
		}
		v.Detail = fmt.Sprintf("variant %d (%s): %s", c.ID, c.Mutation, c.Detail)
	case v.Regression != 0:
		c := v.Regressions[regressionIndex(v.Regressions, v.Regression)]
		v.Status = StatusRegressed
		v.Verdict = fmt.Sprintf("%s(%d)", StatusRegressed, c.ID)
		if c.Seq != nil {
			v.Seq = c.Seq
			v.Verdict += fmt.Sprintf("@%d", *c.Seq)
		}
		v.Detail = fmt.Sprintf("regression variant %d (%s): %s", c.ID, c.Mutation, c.Detail)
	case v.Reproducing < min:
		v.Status, v.Verdict = StatusUnverified, string(StatusUnverified)
		v.Detail = fmt.Sprintf("the recorded incident is fixed, but only %d of %d variants reproduce it on the old build; %d are required", v.Reproducing, v.Candidates, min)
	case allMetadataOnlyReproducing(v.Variants):
		v.Status, v.Verdict = StatusUnverified, string(StatusUnverified)
		v.Detail = "all reproducing variants only mutated non-semantic metadata fields"
	case opts.MinKinds > 0 && len(v.ReproducingByKind) < opts.MinKinds:
		v.Status, v.Verdict = StatusUnverified, string(StatusUnverified)
		v.Detail = fmt.Sprintf("the recorded incident is fixed, but only %d distinct mutation kinds reproduced it on the old build; %d are required", len(v.ReproducingByKind), opts.MinKinds)
	default:
		v.Status, v.Verdict = StatusFixed, string(StatusFixed)
	}
	return v, nil
}

// verifyHealthyBaselineFaults runs driver-level fault-injection + redelivery
// variants against a healthy baseline capsule containing Gateway records.
// If the old build fails under injected driver faults (e.g. double-emit on
// retry caught by invariants.yaml) and the new build passes >= min such
// variants cleanly with no regressions, the verdict is StatusFixed.
func verifyHealthyBaselineFaults(
	j *journal.Journal,
	v *Verification,
	replayOld, replayNew ReplayFunc,
	opts VerifyOptions,
	min int,
	declRules []kconfig.InvariantRule,
) (*Verification, error) {
	limit := MaxCandidates
	if min*3 > limit {
		limit = min * 3
	}
	bundles, err := baselineFaultBundles(j, limit)
	if err != nil {
		return nil, err
	}
	if len(bundles) == 0 {
		v.Status, v.Verdict, v.Seq, v.Detail = v.New.Status, v.New.String(), v.New.Seq, v.New.Detail
		return v, nil
	}
	v.Candidates = len(bundles)
	checks := make([]VariantCheck, len(bundles))
	regPerBundle := make([]*RegressionCheck, len(bundles))
	if err := parallel(len(bundles), opts.Parallel, func(i int) error {
		b := bundles[i]
		info := b.journal.Header.Meta.Variant
		c := VariantCheck{ID: info.ID, Mutation: info.Mutation, Kind: info.Kind, Incident: info.Incident, Journal: b.journal}
		o, err := replayOld(b.journal)
		if err != nil {
			return fmt.Errorf("old build, variant %d: %w", info.ID, err)
		}
		applyDeclarativeInvariants(o, declRules, b.baseline)
		c.Old = o.String()
		if !o.Passed() {
			// Ignore variants where the old build failed or dropped outputs
			// (e.g. rejected a mutated/malformed input) before the injected
			// gateway fault was ever triggered.
			if !anyFaultInjected(o) {
				checks[i] = c
				return nil
			}
			c.Reproduces = true
			n, err := replayNew(b.journal)
			if err != nil {
				return fmt.Errorf("new build, variant %d: %w", info.ID, err)
			}
			applyDeclarativeInvariants(n, declRules, b.baseline)
			c.New = n.String()
			judgeVariant(&c, b.journal, o, n)
			checks[i] = c
			return nil
		}
		checks[i] = c
		if opts.Regressions < 0 {
			return nil
		}
		// Old build survived this fault schedule: verify new build does not
		// regress on it either.
		rc := RegressionCheck{ID: info.ID, Mutation: info.Mutation, Input: info.Incident, Counted: true, Old: o.String(), Journal: b.journal}
		n, err := replayNew(b.journal)
		if err != nil {
			return fmt.Errorf("new build, fault regression variant %d: %w", info.ID, err)
		}
		applyDeclarativeInvariants(n, declRules, b.baseline)
		rc.New = n.String()
		if !n.Passed() {
			rc.Seq, rc.Detail = n.Seq, n.String()
			if n.Detail != "" {
				rc.Detail += ": " + n.Detail
			}
			regPerBundle[i] = &rc
			return nil
		}
		stopSeq, hasStart := faultAttemptStartSeq(b.journal)
		for idx, ost := range o.Steps {
			if hasStart && ost.Seq >= stopSeq {
				break
			}
			if idx >= len(n.Steps) {
				break
			}
			if d := diffBuilds(ost.Outputs, n.Steps[idx].Outputs); d != "" {
				seq := ost.Seq
				rc.Seq, rc.Detail = &seq, "the new build's outputs differ from the old build's on a fault-injection variant: "+d
				regPerBundle[i] = &rc
				return nil
			}
		}
		rc.Passed = true
		regPerBundle[i] = &rc
		return nil
	}); err != nil {
		return nil, err
	}

	var byKind map[string]int
	for _, c := range checks {
		if c.Reproduces {
			v.Reproducing++
			if c.Kind != "" {
				if byKind == nil {
					byKind = map[string]int{}
				}
				byKind[c.Kind]++
			}
		}
		if c.Passed {
			v.Passing++
		} else if c.Reproduces && v.Variant == 0 {
			v.Variant, v.Seq = c.ID, c.Seq
		}
	}
	v.ReproducingByKind = byKind
	v.Variants = checks

	for _, rc := range regPerBundle {
		if rc == nil {
			continue
		}
		v.Regressions = append(v.Regressions, *rc)
		v.RegressionCandidates++
		if rc.Counted {
			v.RegressionCounted++
		}
		if rc.Passed {
			v.RegressionPassing++
		} else if rc.Counted && v.Regression == 0 {
			v.Regression = rc.ID
		}
	}

	switch {
	case v.Variant != 0:
		c := v.Variants[indexOf(v.Variants, v.Variant)]
		v.Status = StatusVariantFailed
		v.Verdict = fmt.Sprintf("%s(%d)", StatusVariantFailed, c.ID)
		if c.Seq != nil {
			v.Verdict += fmt.Sprintf("@%d", *c.Seq)
		}
		v.Detail = fmt.Sprintf("variant %d (%s): %s", c.ID, c.Mutation, c.Detail)
	case v.Regression != 0:
		c := v.Regressions[regressionIndex(v.Regressions, v.Regression)]
		v.Status = StatusRegressed
		v.Verdict = fmt.Sprintf("%s(%d)", StatusRegressed, c.ID)
		if c.Seq != nil {
			v.Seq = c.Seq
			v.Verdict += fmt.Sprintf("@%d", *c.Seq)
		}
		v.Detail = fmt.Sprintf("regression variant %d (%s): %s", c.ID, c.Mutation, c.Detail)
	case v.Reproducing > 0 && v.Reproducing < min:
		v.Status, v.Verdict = StatusUnverified, string(StatusUnverified)
		v.Detail = fmt.Sprintf("fault-injection variants found a bug fixed by the new build, but only %d of %d variants reproduce it on the old build; %d are required", v.Reproducing, v.Candidates, min)
	case v.Reproducing > 0:
		v.Status, v.Verdict = StatusFixed, string(StatusFixed)
	default:
		v.Status, v.Verdict = StatusOK, string(StatusOK)
	}
	return v, nil
}

func checkVariant(vj *journal.Journal, replayOld, replayNew ReplayFunc, requireIncidentOutput bool) (VariantCheck, error) {
	info := vj.Header.Meta.Variant
	c := VariantCheck{ID: info.ID, Mutation: info.Mutation, Kind: info.Kind, Incident: info.Incident, Journal: vj}
	o, err := replayOld(vj)
	if err != nil {
		return c, fmt.Errorf("old build, variant %d: %w", info.ID, err)
	}
	c.Old = o.String()
	if isFaultInjectionVariant(vj) {
		startSeq, hasStart := faultAttemptStartSeq(vj)
		inWindow := o.Seq != nil && (*o.Seq == info.Incident || (hasStart && *o.Seq >= startSeq && *o.Seq <= info.Incident))
		if o.Passed() || !inWindow || (info.Failure != "" && !failureMatches(failureOf(o), info.Failure)) {
			return c, nil
		}
	} else {
		if o.Seq == nil || *o.Seq != info.Incident || !failureMatches(failureOf(o), info.Failure) || (requireIncidentOutput && !hasIncidentOutputs(o)) {
			return c, nil
		}
	}
	c.Reproduces = true
	n, err := replayNew(vj)
	if err != nil {
		return c, fmt.Errorf("new build, variant %d: %w", info.ID, err)
	}
	c.New = n.String()
	judgeVariant(&c, vj, o, n)
	return c, nil
}

func judgeVariant(c *VariantCheck, vj *journal.Journal, old, new *Result) {
	if !new.Passed() {
		c.Seq, c.Detail = new.Seq, new.String()
		if new.Detail != "" {
			c.Detail += ": " + new.Detail
		}
		return
	}
	stopSeq := c.Incident
	if isFaultInjectionVariant(vj) {
		if startSeq, ok := faultAttemptStartSeq(vj); ok {
			stopSeq = startSeq
		}
	}
	for i, o := range old.Steps {
		if o.Seq >= stopSeq {
			break
		}
		if i >= len(new.Steps) {
			break
		}
		if d := diffBuilds(o.Outputs, new.Steps[i].Outputs); d != "" {
			seq := o.Seq
			c.Seq, c.Detail = &seq, "before the failing input, the new build's outputs differ from the old build's: "+d
			return
		}
	}
	c.Passed = true
}

// diffBuilds is diffOutputs for two replays of a variant, which records no
// outputs: it names the builds rather than a recording.
func diffBuilds(old, new []kavach.Output) string {
	return diffNamed(old, new, "old build", "new build")
}

func anyFaultInjected(r *Result) bool {
	if r == nil {
		return false
	}
	for _, st := range r.Steps {
		if st.FaultsInjected > 0 {
			return true
		}
	}
	return false
}

func hasIncidentOutputs(r *Result) bool {
	if r == nil || r.Seq == nil || len(r.Steps) == 0 {
		return false
	}
	for i := len(r.Steps) - 1; i >= 0; i-- {
		if r.Steps[i].Seq == *r.Seq {
			return len(r.Steps[i].Outputs) > 0
		}
	}
	return false
}

func failureOf(r *Result) string {
	switch r.Status {
	case StatusStillFailing:
		last := r.Steps[len(r.Steps)-1]
		switch {
		case last.Crash != "":
			return journal.MarkerCrash
		case last.Timeout != "":
			return "timeout"
		}
		if last.Panic != "" {
			return journal.MarkerPanic + ": " + last.Panic
		}
		return journal.MarkerError + ": " + last.Error
	case StatusInvariantViolated:
		return "invariant: " + r.Invariant
	}
	return ""
}

func indexOf(cs []VariantCheck, id int) int {
	for i, c := range cs {
		if c.ID == id {
			return i
		}
	}
	return -1
}

func parallel(n, par int, fn func(i int) error) error {
	errs := make([]error, n)
	sem := make(chan struct{}, max(par, 1))
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			errs[i] = fn(i)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func checkRegression(vj *journal.Journal, replayOld, replayNew ReplayFunc) (RegressionCheck, error) {
	info := vj.Header.Meta.Variant
	c := RegressionCheck{ID: info.ID, Mutation: info.Mutation, Input: info.Incident, Journal: vj}
	o, err := replayOld(vj)
	if err != nil {
		return c, fmt.Errorf("old build, regression variant %d: %w", info.ID, err)
	}
	c.Old = o.String()
	if o.Status != StatusOK {
		return c, nil
	}
	c.Counted = true
	n, err := replayNew(vj)
	if err != nil {
		return c, fmt.Errorf("new build, regression variant %d: %w", info.ID, err)
	}
	c.New = n.String()
	if n.Status != StatusOK {
		c.Seq, c.Detail = n.Seq, n.String()
		if n.Detail != "" {
			c.Detail += ": " + n.Detail
		}
		return c, nil
	}
	for i, ost := range o.Steps {
		if i >= len(n.Steps) {
			break
		}
		if d := diffBuilds(ost.Outputs, n.Steps[i].Outputs); d != "" {
			seq := ost.Seq
			c.Seq, c.Detail = &seq, "the new build's outputs differ from the old build's, which replays the variant as ok: "+d
			return c, nil
		}
	}
	c.Passed = true
	return c, nil
}

func regressionIndex(cs []RegressionCheck, id int) int {
	for i, c := range cs {
		if c.ID == id {
			return i
		}
	}
	return -1
}

var failureLiteralRE = regexp.MustCompile(
	`(^|[^0-9A-Za-z_])(?:` +
		`(\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[Zz]|[+-]\d{2}:\d{2}))|` +
		`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})|` +
		`(0[xX][0-9a-fA-F]+|[0-9a-fA-F]{16,})|` +
		`([+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)` +
		`)\b`,
)

// failureMatches reports whether an observed failure matches the recorded one.
// Crash, timeout, and invariant failures must match strictly; error and panic
// messages are compared after normalizing variable literals so numeric/time
// mutations still count as reproducing the same failure mode.
func failureMatches(actual, expected string) bool {
	if actual == expected {
		return true
	}
	if (strings.HasPrefix(actual, journal.MarkerError+": ") && strings.HasPrefix(expected, journal.MarkerError+": ")) ||
		(strings.HasPrefix(actual, journal.MarkerPanic+": ") && strings.HasPrefix(expected, journal.MarkerPanic+": ")) {
		return normalizeFailure(actual) == normalizeFailure(expected)
	}
	return false
}

// normalizeFailure preserves an error/panic prefix and canonicalizes RFC3339
// timestamps, UUIDs, hex hashes, and numeric literals inside the message.
func normalizeFailure(s string) string {
	var prefix, body string
	switch {
	case strings.HasPrefix(s, journal.MarkerError+": "):
		prefix = journal.MarkerError + ": "
		body = s[len(prefix):]
	case strings.HasPrefix(s, journal.MarkerPanic+": "):
		prefix = journal.MarkerPanic + ": "
		body = s[len(prefix):]
	default:
		return s
	}
	matches := failureLiteralRE.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(prefix)
	pos := 0
	for _, m := range matches {
		b.WriteString(body[pos:m[3]])
		switch {
		case m[4] >= 0:
			b.WriteString("<ts>")
		case m[6] >= 0:
			b.WriteString("<uuid>")
		case m[8] >= 0:
			b.WriteString("<hex>")
		case m[10] >= 0:
			b.WriteString("<num>")
		}
		pos = m[1]
	}
	b.WriteString(body[pos:])
	return b.String()
}

func allMetadataOnlyReproducing(checks []VariantCheck) bool {
	reproducing := 0
	for _, c := range checks {
		if !c.Reproduces {
			continue
		}
		reproducing++
		if c.Kind != KindField && c.Kind != KindPair && c.Kind != "" {
			return false
		}
		if !isMetadataOnlyMutation(c.Mutation) {
			return false
		}
	}
	return reproducing > 0
}

func isMetadataOnlyMutation(mutation string) bool {
	s := strings.TrimSpace(mutation)
	if strings.HasPrefix(s, "failing input ") {
		i := strings.Index(s, ": ")
		if i < 0 {
			return false
		}
		s = s[i+2:]
	}
	if s == "" {
		return false
	}
	clauses := 0
	for {
		s = strings.TrimSpace(s)
		key, rest, ok := parseMutationKey(s)
		if !ok || !IsMetadataKey(key) {
			return false
		}
		clauses++
		s = rest
		switch {
		case strings.HasPrefix(s, " removed"):
			s = s[len(" removed"):]
		case strings.HasPrefix(s, ": "):
			var consumed bool
			s, consumed = consumeMutationValue(s[2:])
			if !consumed || !strings.HasPrefix(s, " added") {
				return false
			}
			s = s[len(" added"):]
		case strings.HasPrefix(s, " "):
			var consumed bool
			s, consumed = consumeMutationValue(s[1:])
			if !consumed || !strings.HasPrefix(s, " → ") {
				return false
			}
			s, consumed = consumeMutationValue(s[len(" → "):])
			if !consumed {
				return false
			}
		default:
			return false
		}
		if s == "" {
			return clauses > 0
		}
		if !strings.HasPrefix(s, ", ") {
			return false
		}
		s = s[2:]
	}
}

func parseMutationKey(s string) (key, rest string, ok bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", false
	}
	end := 1
	escaped := false
	for end < len(s) {
		ch := s[end]
		if escaped {
			escaped = false
			end++
			continue
		}
		if ch == '\\' {
			escaped = true
			end++
			continue
		}
		if ch == '"' {
			break
		}
		end++
	}
	if end >= len(s) || s[end] != '"' {
		return "", "", false
	}
	k, err := strconv.Unquote(s[:end+1])
	if err != nil {
		return "", "", false
	}
	key = k
	rest = s[end+1:]
	for {
		if strings.HasPrefix(rest, "[") {
			closeIdx := strings.IndexByte(rest, ']')
			if closeIdx < 0 {
				return "", "", false
			}
			rest = rest[closeIdx+1:]
			continue
		}
		if strings.HasPrefix(rest, ".") {
			sub := rest[1:]
			if strings.HasPrefix(sub, `"`) {
				subKey, nextRest, subOK := parseMutationKey(sub)
				if !subOK {
					return "", "", false
				}
				key = subKey
				rest = nextRest
				continue
			}
			stop := strings.IndexAny(sub, " :.[,")
			if stop < 0 {
				stop = len(sub)
			}
			if stop == 0 {
				return "", "", false
			}
			key = sub[:stop]
			rest = sub[stop:]
			continue
		}
		break
	}
	return key, rest, true
}

func consumeMutationValue(s string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return "", false
	}
	off := int(dec.InputOffset())
	if off < 0 || off > len(s) {
		return "", false
	}
	return s[off:], true
}

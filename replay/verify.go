package replay

import (
	"fmt"
	"sync"

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
	// Parallel is how many variants are checked at once. Values below 1 mean
	// one. The result does not depend on it.
	Parallel int
	// Regressions is how many regression variants (see RegressionVariants) are
	// generated. Zero means DefaultRegressions; a negative value skips them.
	Regressions int
}

// ReplayFunc replays a journal against one build of a handler.
type ReplayFunc func(j *journal.Journal) (*Result, error)

// VariantCheck is how one variant of the incident fared.
type VariantCheck struct {
	ID       int    `json:"id"`
	Mutation string `json:"mutation"`
	Incident uint64 `json:"incident_seq"`
	// Reproduces is true when the old build fails on the variant at
	// Incident exactly as it failed in production. Only those variants count.
	Reproduces bool    `json:"reproduces"`
	Old        string  `json:"old"`
	New        string  `json:"new,omitempty"`
	Passed     bool    `json:"passed"`
	Seq        *uint64 `json:"seq,omitempty"` // where the new build went wrong
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
	Seq     *uint64 `json:"seq,omitempty"` // where the new build's outputs first differ
	Detail  string  `json:"detail,omitempty"`
	// Journal is the variant itself.
	Journal *journal.Journal `json:"-"`
}

// Verification is the outcome of checking a candidate fix against an incident.
type Verification struct {
	Status  Status  `json:"status"`
	Verdict string  `json:"verdict"`
	Variant int     `json:"variant,omitempty"` // the first failing variant
	Seq     *uint64 `json:"seq,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Old and New are the two builds' replays of the recorded journal.
	Old *Result `json:"old"`
	New *Result `json:"new"`
	// MinVariants is the number of reproducing variants required.
	MinVariants int            `json:"min_variants"`
	Candidates  int            `json:"candidates"`
	Reproducing int            `json:"reproducing"`
	Passing     int            `json:"passing"`
	Variants    []VariantCheck `json:"variants"`

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
	return VerifyWith(j,
		func(j *journal.Journal) (*Result, error) { return Run(j, oldHandler) },
		func(j *journal.Journal) (*Result, error) { return Run(j, newHandler) },
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
	oldRes, err := replayOld(j)
	if err != nil {
		return nil, fmt.Errorf("old build: %w", err)
	}
	newRes, err := replayNew(j)
	if err != nil {
		return nil, fmt.Errorf("new build: %w", err)
	}
	v := &Verification{Old: oldRes, New: newRes, MinVariants: max(min, 0), Variants: []VariantCheck{}, Regressions: []RegressionCheck{}}
	if !newRes.Passed() || newRes.Status == StatusOK || min < 0 {
		v.Status, v.Verdict, v.Seq, v.Detail = newRes.Status, newRes.String(), newRes.Seq, newRes.Detail
		return v, nil
	}

	variants, err := Variants(j)
	if err != nil {
		return nil, err
	}
	v.Candidates = len(variants)
	checks := make([]VariantCheck, len(variants))
	if err := parallel(len(variants), opts.Parallel, func(i int) (err error) {
		checks[i], err = checkVariant(variants[i], replayOld, replayNew)
		return err
	}); err != nil {
		return nil, err
	}
	for _, c := range checks {
		if c.Reproduces {
			v.Reproducing++
		}
		if c.Passed {
			v.Passing++
		} else if c.Reproduces && v.Variant == 0 {
			v.Variant, v.Seq = c.ID, c.Seq
		}
	}
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
		rchecks[i], err = checkRegression(regs[i], replayOld, replayNew)
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
	default:
		v.Status, v.Verdict = StatusFixed, string(StatusFixed)
	}
	return v, nil
}

func checkVariant(vj *journal.Journal, replayOld, replayNew ReplayFunc) (VariantCheck, error) {
	info := vj.Header.Meta.Variant
	c := VariantCheck{ID: info.ID, Mutation: info.Mutation, Incident: info.Incident, Journal: vj}
	o, err := replayOld(vj)
	if err != nil {
		return c, fmt.Errorf("old build, variant %d: %w", info.ID, err)
	}
	c.Old = o.String()
	if o.Seq == nil || *o.Seq != info.Incident || failureOf(o) != info.Failure {
		return c, nil
	}
	c.Reproduces = true
	n, err := replayNew(vj)
	if err != nil {
		return c, fmt.Errorf("new build, variant %d: %w", info.ID, err)
	}
	c.New = n.String()
	judge(&c, o, n)
	return c, nil
}

// judge decides whether the new build handled a reproducing variant.
func judge(c *VariantCheck, old, new *Result) {
	if !new.Passed() {
		c.Seq, c.Detail = new.Seq, new.String()
		if new.Detail != "" {
			c.Detail += ": " + new.Detail
		}
		return
	}
	for i, o := range old.Steps {
		if o.Seq >= c.Incident {
			break
		}
		if i >= len(new.Steps) {
			break // cannot happen: the new build ran every step
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

// failureOf describes how a replay failed, in the form of journal.Variant.Failure.
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

// parallel runs fn(0..n-1), at most par at once, and returns the first error
// by index.
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
			break // cannot happen: the new build ran every step
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

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
	// StatusUnverified: the fix passes the recorded incident, but too few
	// variants reproduce the incident on the old build to check it further.
	StatusUnverified Status = "unverified"
)

// DefaultMinVariants is the number of variants a fix must pass.
const DefaultMinVariants = 10

// VerifyOptions configure Verify.
type VerifyOptions struct {
	// MinVariants is how many variants must reproduce the incident on the old
	// build, and then pass on the new one, for a fix to count. Zero means
	// DefaultMinVariants; a negative value skips variants.
	MinVariants int
	// Parallel is how many variants are checked at once. Values below 1 mean
	// one. The result does not depend on it.
	Parallel int
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
//     outputs.
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
	v := &Verification{Old: oldRes, New: newRes, MinVariants: max(min, 0), Variants: []VariantCheck{}}
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
	errs := make([]error, len(variants))
	sem := make(chan struct{}, max(opts.Parallel, 1))
	var wg sync.WaitGroup
	for i, vj := range variants {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			checks[i], errs[i] = checkVariant(vj, replayOld, replayNew)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
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

	switch {
	case v.Variant != 0:
		c := v.Variants[indexOf(v.Variants, v.Variant)]
		v.Status = StatusVariantFailed
		v.Verdict = fmt.Sprintf("%s(%d)", StatusVariantFailed, c.ID)
		if c.Seq != nil {
			v.Verdict += fmt.Sprintf("@%d", *c.Seq)
		}
		v.Detail = fmt.Sprintf("variant %d (%s): %s", c.ID, c.Mutation, c.Detail)
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
		if d := diffOutputs(o.Outputs, new.Steps[i].Outputs); d != "" {
			seq := o.Seq
			c.Seq, c.Detail = &seq, "before the failing input, the new build's outputs differ from the old build's: "+d
			return
		}
	}
	c.Passed = true
}

// failureOf describes how a replay failed, in the form of journal.Variant.Failure.
func failureOf(r *Result) string {
	switch r.Status {
	case StatusStillFailing:
		last := r.Steps[len(r.Steps)-1]
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

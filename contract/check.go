package contract

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
)

// ReplayFunc replays a journal against one build, with options.
type ReplayFunc func(j *journal.Journal, opts replay.Options) (*replay.Result, error)

// CheckOptions configure Check.
type CheckOptions struct {
	// Old replays the base build. It may be nil, which skips the baseline.
	Old ReplayFunc
	// New replays the build under test. It is required.
	New ReplayFunc
	// NoVariants skips the variants of the feature steps.
	NoVariants bool
	// Parallel is how many variants are replayed at once.
	Parallel int
	// Build names the build in rounds.jsonl, e.g. its host command.
	Build string
	// Now stamps the round. Zero means time.Now.
	Now time.Time
}

// VariantFailure is a variant of a feature step that the new build failed.
type VariantFailure struct {
	ID       int     `json:"id"`
	Mutation string  `json:"mutation"`
	Feature  uint64  `json:"feature_seq"` // the feature input varied, as a seq of the variant
	Verdict  string  `json:"verdict"`
	Seq      *uint64 `json:"seq,omitempty"`
	Detail   string  `json:"detail,omitempty"`
	File     string  `json:"file,omitempty"`
}

// Report is the outcome of checking a build against a contract.
type Report struct {
	// Verdict is "pass", or the first problem: baseline_mismatch(V),
	// missing_invariant(X), regression@N, still_failing@N,
	// invariant_violated(X)@N, nondeterministic@N, variant_failed(K)@N,
	// unreached@N, unapproved or stale_lock.
	Verdict string `json:"verdict"`
	Passed  bool   `json:"passed"`
	Detail  string `json:"detail,omitempty"`

	Lock     LockState `json:"lock"`
	Features []uint64  `json:"feature_steps"`
	// Baseline is the base build's replay of the traffic, which must be ok
	// with every feature step unreached.
	Baseline string `json:"baseline,omitempty"`
	// New is the new build's replay status.
	New               string          `json:"new"`
	Changed           []replay.Change `json:"changed"`
	Unreached         []uint64        `json:"unreached"`
	MissingInvariants []string        `json:"missing_invariants,omitempty"`

	VariantCandidates int `json:"variant_candidates"`
	VariantsPassed    int `json:"variants_passed"`
	// VariantsPreexisting failed on the base build the same way, before the
	// feature input, and are not counted against the new build.
	VariantsPreexisting int              `json:"variants_preexisting"`
	VariantFailures     []VariantFailure `json:"variant_failures"`

	// Round is this check's number in rounds.jsonl. Streak is how many
	// rounds in a row, ending with this one, had the same outcome.
	Round  int `json:"round"`
	Streak int `json:"streak"`
}

// Check replays the contract's traffic on the new build, sorts its steps
// (SPEC.md §6.4) and checks variants of the feature steps. It records the
// round in rounds.jsonl and saves failing variants under variants/.
func (d *Dir) Check(o CheckOptions) (*Report, error) {
	r := &Report{Lock: d.Lock(), Features: d.FeatureSeqs(), Changed: []replay.Change{}, Unreached: []uint64{}, VariantFailures: []VariantFailure{}}
	opts := replay.Options{Features: d.Features}
	if o.Old != nil {
		base, err := o.Old(d.Traffic, opts)
		if err != nil {
			return nil, fmt.Errorf("old build: %w", err)
		}
		r.Baseline = base.String()
		switch {
		case !base.Passed():
			r.fail("baseline_mismatch("+base.String()+")", "the base build does not replay the traffic: "+base.Detail)
		case len(base.Changed) > 0:
			r.fail(fmt.Sprintf("baseline_mismatch(changed@%d)", base.Changed[0].Seq), "the base build changes a feature step's recorded outputs: "+base.Changed[0].Detail)
		}
	}
	if r.Verdict == "" {
		res, err := o.New(d.Traffic, opts)
		if err != nil {
			return nil, fmt.Errorf("new build: %w", err)
		}
		r.New = res.String()
		r.Changed = append(r.Changed, res.Changed...)
		r.Unreached = append(r.Unreached, res.Unreached...)
		for _, name := range d.Contract.Invariants {
			if !contains(res.Invariants, name) {
				r.MissingInvariants = append(r.MissingInvariants, name)
			}
		}
		switch {
		case len(r.MissingInvariants) > 0:
			r.fail("missing_invariant("+r.MissingInvariants[0]+")", fmt.Sprintf("the build declares %v", res.Invariants))
		case res.Status == replay.StatusDiverged:
			r.fail(fmt.Sprintf("regression@%d", *res.Seq), res.Detail)
		case !res.Passed():
			r.fail(res.String(), res.Detail)
		}
	}
	if r.Verdict == "" && !o.NoVariants {
		if err := d.checkVariants(r, o); err != nil {
			return nil, err
		}
	}
	switch {
	case r.Verdict != "":
	case len(r.Unreached) > 0:
		r.fail(fmt.Sprintf("unreached@%d", r.Unreached[0]), "the feature step produced the base build's outputs")
	case r.Lock.State == "unapproved":
		r.fail("unapproved", "the build meets the contract, but the contract has not been approved")
	case r.Lock.State == "stale":
		r.fail("stale_lock", fmt.Sprintf("the build meets the contract, but these files changed since it was approved: %v", r.Lock.Changed))
	default:
		r.Verdict, r.Passed = "pass", true
	}
	if err := d.addRound(r, o); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Report) fail(verdict, detail string) {
	if r.Verdict == "" {
		r.Verdict, r.Detail = verdict, detail
	}
}

func (d *Dir) checkVariants(r *Report, o CheckOptions) error {
	vs, err := replay.FeatureVariants(d.Traffic, d.Features)
	if err != nil {
		return err
	}
	r.VariantCandidates = len(vs)
	results := make([]*replay.Result, len(vs))
	errs := make([]error, len(vs))
	sem := make(chan struct{}, max(o.Parallel, 1))
	var wg sync.WaitGroup
	for i, vj := range vs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			results[i], errs[i] = o.New(vj, replay.Options{})
		}()
	}
	wg.Wait()
	vdir := filepath.Join(d.Path, DirVariants)
	os.RemoveAll(vdir)
	for i, res := range results {
		if errs[i] != nil {
			return fmt.Errorf("new build, variant %d: %w", i+1, errs[i])
		}
		if res.Passed() {
			r.VariantsPassed++
			continue
		}
		info := vs[i].Header.Meta.Variant
		// A failure the base build has too, before the feature input, is not
		// the new build's doing: the variant broke behaviour that predates it.
		if o.Old != nil && res.Seq != nil && *res.Seq < info.Incident {
			old, err := o.Old(vs[i], replay.Options{})
			if err != nil {
				return fmt.Errorf("old build, variant %d: %w", info.ID, err)
			}
			if old.String() == res.String() {
				r.VariantsPreexisting++
				continue
			}
		}
		f := VariantFailure{ID: info.ID, Mutation: info.Mutation, Feature: info.Incident, Verdict: res.String(), Seq: res.Seq, Detail: res.Detail}
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			return err
		}
		f.File = filepath.Join(vdir, fmt.Sprintf("variant-%02d.kavach", info.ID))
		if err := journal.WriteFile(f.File, vs[i].Header.Meta, vs[i].Records); err != nil {
			return err
		}
		r.VariantFailures = append(r.VariantFailures, f)
	}
	if len(r.VariantFailures) > 0 {
		f := r.VariantFailures[0]
		v := fmt.Sprintf("variant_failed(%d)", f.ID)
		if f.Seq != nil {
			v += fmt.Sprintf("@%d", *f.Seq)
		}
		r.fail(v, fmt.Sprintf("variant %d (%s): %s", f.ID, f.Mutation, f.Verdict))
	}
	return nil
}

// round is one line of rounds.jsonl.
type round struct {
	Round     int    `json:"round"`
	At        string `json:"at"`
	Build     string `json:"build,omitempty"`
	Verdict   string `json:"verdict"`
	Changed   int    `json:"changed"`
	Signature string `json:"signature"` // equal for checks with the same outcome
}

// signature digests what a build did: the verdict, the changed outputs and
// the failing variants. Two rounds with the same signature made no progress.
func (r *Report) signature() string {
	b, _ := json.Marshal([]any{r.Verdict, r.Changed, r.Unreached, r.VariantFailures})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func (d *Dir) addRound(r *Report, o CheckOptions) error {
	p := filepath.Join(d.Path, FileRounds)
	var prev []round
	if f, err := os.Open(p); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var x round
			if json.Unmarshal(sc.Bytes(), &x) == nil {
				prev = append(prev, x)
			}
		}
		f.Close()
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	cur := round{Round: len(prev) + 1, At: now.UTC().Format(time.RFC3339), Build: o.Build, Verdict: r.Verdict, Changed: len(r.Changed), Signature: r.signature()}
	r.Round, r.Streak = cur.Round, 1
	for i := len(prev) - 1; i >= 0 && prev[i].Signature == cur.Signature; i-- {
		r.Streak++
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(cur)
	_, err = f.Write(append(b, '\n'))
	return err
}

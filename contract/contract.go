// Package contract checks a build of a service against a behaviour contract
// (SPEC.md §6.4): traffic recorded on the build before a feature, the steps
// of that traffic that reach the feature, and the invariants the feature must
// keep. It is the checking half of contract-first feature work; see
// docs/design/implement.md.
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// Files of a contract directory.
const (
	FileContract = "contract.json"
	FileTraffic  = "traffic.kavach"
	FileLock     = "lock.json"
	FileRounds   = "rounds.jsonl"
	DirVariants  = "variants"
)

// DefaultDir is the contract directory used when none is given.
const DefaultDir = "contract"

// Contract is the contents of contract.json. Paths are relative to the
// contract directory.
type Contract struct {
	// Spec is the specification the contract was written from.
	Spec string `json:"spec"`
	// FeatureSteps and FeatureMatch select the feature steps: inputs of the
	// traffic that reach the new behaviour. A step is a feature step if its
	// input's seq is listed, or its input matches any of the expressions
	// (see Match).
	FeatureSteps []uint64 `json:"feature_steps,omitempty"`
	FeatureMatch []string `json:"feature_match,omitempty"`
	// Invariants are the names of invariants the build must declare.
	Invariants []string `json:"invariants,omitempty"`
	// InvariantSources are the files that declare them; the lock covers them.
	InvariantSources []string `json:"invariant_sources,omitempty"`
}

// Dir is a contract directory, loaded.
type Dir struct {
	Path     string
	Contract Contract
	Traffic  *journal.Journal
	// Features are the seqs of the feature steps' inputs.
	Features map[uint64]bool
}

// InitOptions describe a new contract.
type InitOptions struct {
	Spec             string // path to the specification
	Traffic          string // path to the journal recorded on the base build
	FeatureSteps     []StepRange
	FeatureMatch     []string
	Invariants       []string
	InvariantSources []string
	Force            bool // replace an existing contract
}

// Init creates a contract in dir: it copies the traffic in and writes
// contract.json. It does not approve the contract.
func Init(dir string, o InitOptions) (*Dir, error) {
	if o.Spec == "" || o.Traffic == "" {
		return nil, errors.New("a spec and a traffic journal are required")
	}
	if len(o.FeatureSteps) == 0 && len(o.FeatureMatch) == 0 {
		return nil, errors.New("no feature steps: give their seqs or a match expression")
	}
	if _, err := os.Stat(filepath.Join(dir, FileContract)); err == nil && !o.Force {
		return nil, fmt.Errorf("%s already holds a contract; pass --force to replace it", dir)
	}
	for _, p := range append([]string{o.Spec}, o.InvariantSources...) {
		if _, err := os.Stat(p); err != nil {
			return nil, err
		}
	}
	j, err := journal.ReadFile(o.Traffic)
	if err != nil {
		return nil, fmt.Errorf("traffic: %w", err)
	}
	if j.Header.Meta.Variant != nil {
		return nil, errors.New("traffic: a variant is not a recording")
	}
	c := Contract{FeatureMatch: o.FeatureMatch, Invariants: o.Invariants}
	if c.FeatureSteps, err = inputsIn(j, o.FeatureSteps); err != nil {
		return nil, err
	}
	if c.Spec, err = relTo(dir, o.Spec); err != nil {
		return nil, err
	}
	for _, p := range o.InvariantSources {
		r, err := relTo(dir, p)
		if err != nil {
			return nil, err
		}
		c.InvariantSources = append(c.InvariantSources, r)
	}
	features, err := selectFeatures(c, j)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := copyFile(o.Traffic, filepath.Join(dir, FileTraffic)); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, FileContract), c); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(dir, FileLock))
	os.Remove(filepath.Join(dir, FileRounds))
	return &Dir{Path: dir, Contract: c, Traffic: j, Features: features}, nil
}

// Load reads the contract in dir.
func Load(dir string) (*Dir, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileContract))
	if err != nil {
		return nil, err
	}
	var c Contract
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", FileContract, err)
	}
	j, err := journal.ReadFile(filepath.Join(dir, FileTraffic))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FileTraffic, err)
	}
	features, err := selectFeatures(c, j)
	if err != nil {
		return nil, err
	}
	return &Dir{Path: dir, Contract: c, Traffic: j, Features: features}, nil
}

// FeatureSeqs returns the feature steps' seqs in order.
func (d *Dir) FeatureSeqs() []uint64 {
	var out []uint64
	for s := range d.Features {
		out = append(out, s)
	}
	sort.Slice(out, func(i, k int) bool { return out[i] < out[k] })
	return out
}

func selectFeatures(c Contract, j *journal.Journal) (map[uint64]bool, error) {
	inputs := map[uint64]journal.Record{}
	for _, r := range j.Records {
		if r.Type == journal.TypeInput {
			inputs[r.Seq] = r
		}
	}
	features := map[uint64]bool{}
	for _, s := range c.FeatureSteps {
		if _, ok := inputs[s]; !ok {
			return nil, fmt.Errorf("feature step %d: the traffic has no input with that seq", s)
		}
		features[s] = true
	}
	for _, expr := range c.FeatureMatch {
		m, err := ParseMatch(expr)
		if err != nil {
			return nil, err
		}
		n := 0
		for s, r := range inputs {
			if m.Matches(r.Data) {
				features[s] = true
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("feature match %s selects no input of the traffic", expr)
		}
	}
	if len(features) == 0 {
		return nil, errors.New("the contract selects no feature steps")
	}
	return features, nil
}

// StepRange is a range of seqs, inclusive. A range of one seq names an
// input; a longer one selects the inputs within it.
type StepRange struct{ Lo, Hi uint64 }

// inputsIn returns the seqs of j's inputs that the ranges select.
func inputsIn(j *journal.Journal, rs []StepRange) ([]uint64, error) {
	inputs := map[uint64]bool{}
	for _, r := range j.Records {
		if r.Type == journal.TypeInput {
			inputs[r.Seq] = true
		}
	}
	var out []uint64
	for _, r := range rs {
		n := 0
		for s := r.Lo; s <= r.Hi; s++ {
			if inputs[s] {
				out = append(out, s)
				n++
			}
		}
		switch {
		case n == 0 && r.Lo == r.Hi:
			return nil, fmt.Errorf("feature step %d: the traffic has no input with that seq", r.Lo)
		case n == 0:
			return nil, fmt.Errorf("feature steps %d-%d: the traffic has no input in that range", r.Lo, r.Hi)
		}
	}
	return out, nil
}

// ParseSteps parses a list of seqs and ranges, such as "40-52,60".
func ParseSteps(s string) ([]StepRange, error) {
	var out []StepRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("feature steps: %q is not a seq or a range", part)
		}
		b := a
		if isRange {
			if b, err = strconv.ParseUint(strings.TrimSpace(hi), 10, 64); err != nil || b < a {
				return nil, fmt.Errorf("feature steps: %q is not a seq or a range", part)
			}
		}
		out = append(out, StepRange{a, b})
	}
	return out, nil
}

// Lock is the contents of lock.json: what was approved.
type Lock struct {
	ApprovedAt string `json:"approved_at"`
	// Interactive is true when a person confirmed the approval at a terminal,
	// false when it was given with --yes.
	Interactive bool `json:"interactive"`
	// Files maps each locked file, relative to the contract directory, to the
	// SHA-256 of its contents.
	Files map[string]string `json:"files"`
}

// LockState is whether the contract is approved and unchanged since.
type LockState struct {
	State       string   `json:"state"` // "approved", "unapproved" or "stale"
	Interactive bool     `json:"interactive,omitempty"`
	Changed     []string `json:"changed,omitempty"` // files changed since approval
}

func (d *Dir) lockedFiles() []string {
	files := []string{FileContract, FileTraffic, d.Contract.Spec}
	return append(files, d.Contract.InvariantSources...)
}

// Approve locks the contract as it is now.
func (d *Dir) Approve(interactive bool, now time.Time) (*Lock, error) {
	l := &Lock{ApprovedAt: now.UTC().Format(time.RFC3339), Interactive: interactive, Files: map[string]string{}}
	for _, f := range d.lockedFiles() {
		h, err := hashFile(filepath.Join(d.Path, f))
		if err != nil {
			return nil, err
		}
		l.Files[f] = h
	}
	return l, writeJSON(filepath.Join(d.Path, FileLock), l)
}

// Lock reports the state of the contract's lock.
func (d *Dir) Lock() LockState {
	b, err := os.ReadFile(filepath.Join(d.Path, FileLock))
	if err != nil {
		return LockState{State: "unapproved"}
	}
	var l Lock
	if json.Unmarshal(b, &l) != nil {
		return LockState{State: "stale", Changed: []string{FileLock}}
	}
	st := LockState{State: "approved", Interactive: l.Interactive}
	files := d.lockedFiles()
	for f := range l.Files {
		if !contains(files, f) {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	for _, f := range files {
		h, err := hashFile(filepath.Join(d.Path, f))
		if err != nil || h != l.Files[f] {
			st.Changed = append(st.Changed, f)
		}
	}
	if len(st.Changed) > 0 {
		st.State = "stale"
	}
	return st
}

func relTo(dir, p string) (string, error) {
	ad, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	ap, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Rel(ad, ap)
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

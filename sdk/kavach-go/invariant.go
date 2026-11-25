package kavach

import (
	"errors"
	"fmt"

	"github.com/kavachlabs/kavach-go/invariants"
)

// InvariantFunc is a user-registered Go invariant checked at the end of every step.
type InvariantFunc func(Step) error

type codeInvariant struct {
	name string
	fn   InvariantFunc
}

// InvariantError is returned when a code or declarative YAML invariant fails.
type InvariantError struct {
	Name string
	Kind string
	Err  error
}

// Error implements error.
func (e *InvariantError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("kavach: invariant %s violated", e.Name)
	}
	return fmt.Sprintf("kavach: invariant %s violated: %v", e.Name, e.Err)
}

// Unwrap returns the underlying cause error.
func (e *InvariantError) Unwrap() error { return e.Err }

// Invariant registers a named Go code invariant evaluated at the end of every step.
func (kv *Kavach) Invariant(name string, fn func(Step) error) {
	if kv == nil || name == "" || fn == nil {
		return
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.codeInvs = append(kv.codeInvs, codeInvariant{name: name, fn: fn})
}

// LoadInvariants parses and registers declarative rules from a .kavach/invariants.yaml file.
func (kv *Kavach) LoadInvariants(path string) error {
	if kv == nil {
		return nil
	}
	f, err := invariants.LoadFile(path)
	if err != nil {
		return err
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	all := append(kv.eval.Rules(), f.Invariants...)
	kv.eval = invariants.NewEvaluator(all)
	return nil
}

// AddInvariantRule registers an individual declarative invariant rule programmatically.
func (kv *Kavach) AddInvariantRule(rule invariants.Rule) {
	if kv == nil {
		return
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	all := append(kv.eval.Rules(), rule)
	kv.eval = invariants.NewEvaluator(all)
}

// InvariantNames returns the names of all registered declarative YAML and code invariants.
func (kv *Kavach) InvariantNames() []string {
	if kv == nil {
		return []string{}
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	names := make([]string, 0, len(kv.codeInvs)+len(kv.eval.Rules()))
	names = append(names, kv.eval.RuleNames()...)
	for _, c := range kv.codeInvs {
		names = append(names, c.name)
	}
	return names
}

// Evaluator returns the underlying declarative invariants Evaluator.
func (kv *Kavach) Evaluator() *invariants.Evaluator {
	if kv == nil {
		return nil
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.eval
}

func (kv *Kavach) checkInvariants(sc *StepContext) error {
	kv.mu.Lock()
	eval := kv.eval
	codeInvs := append([]codeInvariant(nil), kv.codeInvs...)
	kv.mu.Unlock()

	snap := sc.snapshot()

	// Always evaluate declarative YAML rules: for failing steps this records
	// emitted keys (so a subsequent retry catches duplicates) AND checks any
	// explicit on_error_emit_count limit.
	if v := eval.CheckStep(snap); v != nil {
		return &InvariantError{Name: v.Name, Kind: v.Kind, Err: errors.New(v.Detail)}
	}

	// Code invariants run after non-panicking steps; if the step errored, we still
	// run code invariants only on clean steps (matching SPEC §9) unless they check
	// error steps via declarative rules. Wait: if a user registers a code invariant
	// calling s.AssertAtMostOnceEmit, we want attempt 1's emits tracked even if
	// attempt 1 returned an error, AND we want normal invariants checked when err == nil.
	if snap.Panicked {
		return nil
	}
	for _, inv := range codeInvs {
		if err := inv.fn(sc); err != nil {
			// On a step that already returned an error, only escalate invariant failure
			// if it explicitly failed (e.g. duplicate emit or error-step emit check);
			// otherwise let the handler error propagate on normal invariants that assume
			// success state.
			if snap.Err == nil {
				return &InvariantError{Name: inv.name, Kind: "code", Err: err}
			}
		}
	}
	return nil
}

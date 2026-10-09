package replay

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kavachlabs/kavach/internal/envfacts"
	"github.com/kavachlabs/kavach/journal"
)

// Drift is one key whose value in the replay differs from the journal's
// genesis environment (SPEC.md §6.2). Values are shown as text; a hashed fact
// is shown as "sha256:<hex>" and a missing one as "(unset)".
type Drift struct {
	Key      string `json:"key"`
	Recorded string `json:"recorded"`
	Replay   string `json:"replay"`
}

// EnvChange is an environment change record: the facts that changed after the
// step whose input had seq After.
type EnvChange struct {
	After uint64      `json:"after"`
	Facts []FactValue `json:"facts"`
}

// FactValue is a fact of an environment record, shown as text.
type FactValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func newEnvChange(after uint64, env journal.Record) EnvChange {
	c := EnvChange{After: after}
	for _, f := range env.Facts {
		c.Facts = append(c.Facts, FactValue{f.Key, showFact(f)})
	}
	return c
}

func showFact(f journal.Fact) string {
	switch f.Form {
	case journal.FactSHA256:
		return fmt.Sprintf("sha256:%x", f.Value)
	case journal.FactUnset:
		return "(unset)"
	}
	if utf8.Valid(f.Value) {
		return string(f.Value)
	}
	return fmt.Sprintf("%x", f.Value)
}

// sameFact reports whether two facts have the same value, comparing a hashed
// fact with a plain one by hashing the plain one.
func sameFact(a, b journal.Fact) bool {
	if a.Form == journal.FactUnset || b.Form == journal.FactUnset {
		return a.Form == b.Form
	}
	if a.Form == journal.FactValue && b.Form == journal.FactValue {
		return bytes.Equal(a.Value, b.Value)
	}
	hash := func(f journal.Fact) []byte {
		if f.Form == journal.FactValue {
			sum := sha256.Sum256(f.Value)
			return sum[:]
		}
		return f.Value
	}
	return bytes.Equal(hash(a), hash(b))
}

// computeDrift compares the replay's environment with the genesis facts.
// A replay that collected nothing but host.runtime (no kavach-recorder to run)
// is compared on that key alone, and flag. facts are never compared: neither
// was collected, so neither is drift (SPEC.md §9.2).
func computeDrift(genesis []journal.Fact, replay map[string]envfacts.JSONFact) ([]Drift, error) {
	rf, err := envfacts.FromJSON(replay)
	if err != nil {
		return nil, err
	}
	collected := len(rf) > 1 || (len(rf) == 1 && rf[0].Key != "host.runtime")
	recorded := map[string]journal.Fact{}
	keys := map[string]bool{}
	for _, f := range genesis {
		recorded[f.Key], keys[f.Key] = f, true
	}
	got := map[string]journal.Fact{}
	for _, f := range rf {
		got[f.Key], keys[f.Key] = f, true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []Drift
	for _, k := range sorted {
		if strings.HasPrefix(k, "flag.") || (!collected && k != "host.runtime") {
			continue
		}
		unset := journal.Fact{Key: k, Form: journal.FactUnset}
		rec, ok := recorded[k]
		if !ok {
			rec = unset
		}
		rep, ok := got[k]
		if !ok {
			rep = unset
		}
		if !sameFact(rec, rep) {
			out = append(out, Drift{k, showFact(rec), showFact(rep)})
		}
	}
	return out, nil
}

package envfacts

import (
	"encoding/json"
	"fmt"

	"github.com/kavachlabs/kavach/journal"
)

// JSONFact is a fact in the form of `ready.environment` (SPEC.md §9.2):
// {"value": <base64>}, {"sha256": <base64>} or {"unset": true}.
type JSONFact struct {
	Value  *[]byte `json:"value,omitempty"`
	SHA256 *[]byte `json:"sha256,omitempty"`
	Unset  bool    `json:"unset,omitempty"`
}

// ToJSON converts facts to the JSON object `kavach-recorder facts` prints, from
// key to fact.
func ToJSON(facts []journal.Fact) map[string]JSONFact {
	out := make(map[string]JSONFact, len(facts))
	for _, f := range facts {
		v := append([]byte{}, f.Value...)
		switch f.Form {
		case journal.FactSHA256:
			out[f.Key] = JSONFact{SHA256: &v}
		case journal.FactUnset:
			out[f.Key] = JSONFact{Unset: true}
		default:
			out[f.Key] = JSONFact{Value: &v}
		}
	}
	return out
}

// FromJSON is the inverse of ToJSON; the result is sorted by key.
func FromJSON(m map[string]JSONFact) ([]journal.Fact, error) {
	facts := make([]journal.Fact, 0, len(m))
	for k, f := range m {
		n := 0
		if f.Value != nil {
			n++
		}
		if f.SHA256 != nil {
			n++
		}
		if f.Unset {
			n++
		}
		if n != 1 {
			return nil, fmt.Errorf("fact %q must have exactly one of value, sha256 and unset", k)
		}
		switch {
		case f.Value != nil:
			facts = append(facts, journal.Fact{Key: k, Form: journal.FactValue, Value: *f.Value})
		case f.SHA256 != nil:
			if len(*f.SHA256) != 32 {
				return nil, fmt.Errorf("fact %q: sha256 is %d bytes, want 32", k, len(*f.SHA256))
			}
			facts = append(facts, journal.Fact{Key: k, Form: journal.FactSHA256, Value: *f.SHA256})
		default:
			facts = append(facts, journal.Fact{Key: k, Form: journal.FactUnset})
		}
	}
	journal.SortFacts(facts)
	return facts, nil
}

// MarshalFacts encodes facts as indented JSON.
func MarshalFacts(facts []journal.Fact) ([]byte, error) {
	return json.MarshalIndent(ToJSON(facts), "", "  ")
}

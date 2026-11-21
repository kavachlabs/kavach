package kmap

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// DiffSummary summarizes a `kavach diff --json` report for a fixture.
type DiffSummary struct {
	Source   string `json:"source"`
	Verdict  string `json:"verdict"`
	Old      string `json:"old"`
	New      string `json:"new"`
	Variants struct {
		Required    int `json:"required"`
		Candidates  int `json:"candidates"`
		Reproducing int `json:"reproducing"`
		Passing     int `json:"passing"`
	} `json:"variants"`
	Regressions struct {
		Neighbours int `json:"neighbours"`
		Candidates int `json:"candidates"`
		Counted    int `json:"counted"`
		Passing    int `json:"passing"`
	} `json:"regressions"`
	ByKind      map[string]*Tally    `json:"by_kind,omitempty"`
	ByInvariant map[string]*InvTally `json:"by_invariant,omitempty"`
	ByOutcome   map[string]int       `json:"by_outcome,omitempty"`
	Failures    []DiffCheck          `json:"failures,omitempty"`
}

// Tally counts total and passing variants in one bucket.
type Tally struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
}

// InvTally counts invariant violations on old vs new builds in a diff report.
type InvTally struct {
	Old int `json:"old"`
	New int `json:"new"`
}

// DiffCheck is one failing variant or regression check kept in DiffSummary.Failures.
type DiffCheck struct {
	Scope    string `json:"scope"` // "variant" | "regression"
	ID       int    `json:"id"`
	Kind     string `json:"kind,omitempty"`
	Seq      uint64 `json:"seq,omitempty"`
	Mutation string `json:"mutation"`
	Old      string `json:"old"`
	New      string `json:"new,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type rawDiffReport struct {
	Fixture  string `json:"fixture"`
	Verdict  string `json:"verdict"`
	Old      string `json:"old"`
	New      string `json:"new"`
	Variants struct {
		Required    int `json:"required"`
		Candidates  int `json:"candidates"`
		Reproducing int `json:"reproducing"`
		Passing     int `json:"passing"`
		Checks      []struct {
			ID          int    `json:"id"`
			Mutation    string `json:"mutation"`
			IncidentSeq uint64 `json:"incident_seq"`
			Reproduces  bool   `json:"reproduces"`
			Old         string `json:"old"`
			New         string `json:"new"`
			Passed      bool   `json:"passed"`
			Detail      string `json:"detail"`
		} `json:"checks"`
	} `json:"variants"`
	Regressions struct {
		Neighbours int `json:"neighbours"`
		Candidates int `json:"candidates"`
		Counted    int `json:"counted"`
		Passing    int `json:"passing"`
		Checks     []struct {
			ID       int    `json:"id"`
			Mutation string `json:"mutation"`
			InputSeq uint64 `json:"input_seq"`
			Counted  bool   `json:"counted"`
			Old      string `json:"old"`
			New      string `json:"new"`
			Passed   bool   `json:"passed"`
			Detail   string `json:"detail"`
		} `json:"checks"`
	} `json:"regressions"`
}

var (
	reInvVerdict = regexp.MustCompile(`^invariant_violated\(([^)]+)\)`)
	reParen      = regexp.MustCompile(`\([^)]*\)`)
	reAtSeq      = regexp.MustCompile(`@\d+$`)
	reQuoted     = regexp.MustCompile(`"[^"]+"`)
)

// LoadDiff reads a `kavach diff --json` report tolerantly and returns its summary
// along with the fixture field from the report (for basename matching).
func LoadDiff(path string) (*DiffSummary, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var raw rawDiffReport
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", fmt.Errorf("decode diff %s: %w", path, err)
	}

	ds := &DiffSummary{
		Verdict:     raw.Verdict,
		Old:         raw.Old,
		New:         raw.New,
		ByKind:      map[string]*Tally{},
		ByInvariant: map[string]*InvTally{},
		ByOutcome:   map[string]int{},
	}
	ds.Variants.Required = raw.Variants.Required
	ds.Variants.Candidates = raw.Variants.Candidates
	ds.Variants.Reproducing = raw.Variants.Reproducing
	ds.Variants.Passing = raw.Variants.Passing

	ds.Regressions.Neighbours = raw.Regressions.Neighbours
	ds.Regressions.Candidates = raw.Regressions.Candidates
	ds.Regressions.Counted = raw.Regressions.Counted
	ds.Regressions.Passing = raw.Regressions.Passing

	const maxFailures = 20

	for _, c := range raw.Variants.Checks {
		kind := guessVariantKind(c.Mutation)
		if c.Old != "" {
			ds.ByOutcome[stripVerdict(c.Old)]++
		}
		recordInvariantTally(ds.ByInvariant, c.Old, true)
		recordInvariantTally(ds.ByInvariant, c.New, false)

		if c.Reproduces {
			t := ds.ByKind[kind]
			if t == nil {
				t = &Tally{}
				ds.ByKind[kind] = t
			}
			t.Total++
			if c.Passed {
				t.Passed++
			} else if len(ds.Failures) < maxFailures {
				ds.Failures = append(ds.Failures, DiffCheck{
					Scope:    "variant",
					ID:       c.ID,
					Kind:     kind,
					Seq:      c.IncidentSeq,
					Mutation: c.Mutation,
					Old:      c.Old,
					New:      c.New,
					Detail:   c.Detail,
				})
			}
		}
	}

	for _, c := range raw.Regressions.Checks {
		recordInvariantTally(ds.ByInvariant, c.Old, true)
		recordInvariantTally(ds.ByInvariant, c.New, false)
		if c.Counted && !c.Passed && len(ds.Failures) < maxFailures {
			ds.Failures = append(ds.Failures, DiffCheck{
				Scope:    "regression",
				ID:       c.ID,
				Kind:     guessVariantKind(c.Mutation),
				Seq:      c.InputSeq,
				Mutation: c.Mutation,
				Old:      c.Old,
				New:      c.New,
				Detail:   c.Detail,
			})
		}
	}

	return ds, raw.Fixture, nil
}

func guessVariantKind(m string) string {
	switch {
	case strings.Contains(m, "clock shifted"):
		return "clock"
	case strings.Contains(m, "delivered twice"):
		return "duplicate"
	case strings.Contains(m, "earlier, before"):
		return "move"
	case strings.Contains(m, "dropped"):
		return "drop"
	case len(reQuoted.FindAllString(m, -1)) > 2 && strings.Contains(m, ","):
		return "pair"
	default:
		return "field"
	}
}

func stripVerdict(v string) string {
	s := reParen.ReplaceAllString(v, "")
	s = reAtSeq.ReplaceAllString(s, "")
	return s
}

func recordInvariantTally(m map[string]*InvTally, verdict string, isOld bool) {
	if verdict == "" {
		return
	}
	sub := reInvVerdict.FindStringSubmatch(verdict)
	if len(sub) < 2 {
		return
	}
	name := sub[1]
	it := m[name]
	if it == nil {
		it = &InvTally{}
		m[name] = it
	}
	if isOld {
		it.Old++
	} else {
		it.New++
	}
}

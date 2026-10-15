package replay

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/kavachlabs/kavach/journal"
)

const (
	// MaxNeighbours bounds the earlier inputs regression variants are made from.
	MaxNeighbours = 6
	// MaxRegressionCandidates bounds the regression variants generated from one
	// incident. They have a budget of their own: they never use up MaxCandidates.
	MaxRegressionCandidates = 48
)

var regressionKinds = []string{KindRegression}

// regressionWords rewrites candidate descriptions, which speak of the failing
// input, for a neighbour.
var regressionWords = strings.NewReplacer("failing input", "input", "the failing one", "it")

// RegressionVariants derives variants of the inputs before the failing one that
// resemble it (see neighbours), to check that a fix did not change what the
// old build did right on them. Each is the journal up to and including a
// neighbour, given the field and pair mutations of Variants. A variant expects no
// failure: its Meta.Variant.Incident is the seq of the neighbour in the variant
// and its Failure is empty. At most limit variants are built (zero means
// MaxRegressionCandidates), spread evenly over the neighbours.
// Generation is deterministic.
//
// It returns no variants when j recorded no failure or no input resembles the
// failing one.
func RegressionVariants(j *journal.Journal, limit int) ([]*journal.Journal, error) {
	vs, _, err := regressionVariants(j, limit)
	return vs, err
}

// regressionVariants also returns the number of neighbours.
func regressionVariants(j *journal.Journal, limit int) ([]*journal.Journal, int, error) {
	snap, env, steps, err := variantSource(j)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = MaxRegressionCandidates
	}
	var each [][][]candidate
	ns := neighbours(steps)
	for _, k := range ns {
		// Only the input's own values are varied, in one class so that a
		// neighbour's budget goes deep into its fields rather than wide over
		// classes: dropping, moving or redelivering history says little about
		// a fix to one handler path, and clock shifts are already covered by
		// the incident's variants.
		cs := candidates(steps, k)
		cs[0] = append(cs[0], cs[1]...)
		for i := range cs[0] {
			cs[0][i].mutation = regressionWords.Replace(cs[0][i].mutation)
		}
		each = append(each, cs[:1])
	}
	return interleave(j.Header.Meta, snap, env, each, "", regressionKinds, limit), len(ns), nil
}

// neighbours returns the indices of the steps that resemble the first failing
// step, most similar first. A neighbour comes before the failing step, did not
// fail, is a JSON object with the same keys, and shares at least one
// (path, value) leaf with it: for {"argv":[...]} that is at least the command
// name or an argument. Ties go to the later step; inputs identical to an
// already chosen one add nothing and are skipped.
func neighbours(steps []*step) []int {
	f := -1
	for i, st := range steps {
		if st.marker != nil {
			f = i
			break
		}
	}
	if f < 0 {
		return nil
	}
	target, ok := objectFields(steps[f].input.Data)
	if !ok {
		return nil
	}
	want := leaves(steps[f].input.Data)
	type scored struct{ idx, score int }
	var cands []scored
	for i := f - 1; i >= 0; i-- {
		if steps[i].marker != nil || !sameKeys(target, steps[i].input.Data) {
			continue
		}
		n := 0
		for p, v := range leaves(steps[i].input.Data) {
			if w, ok := want[p]; ok && w == v {
				n++
			}
		}
		if n > 0 {
			cands = append(cands, scored{i, n})
		}
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].score > cands[b].score })
	var out []int
	var chosen [][]byte
	for _, c := range cands {
		d := steps[c.idx].input.Data
		dup := false
		for _, x := range chosen {
			if bytes.Equal(x, d) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		chosen = append(chosen, d)
		out = append(out, c.idx)
		if len(out) == MaxNeighbours {
			break
		}
	}
	return out
}

func sameKeys(a []field, data []byte) bool {
	b, ok := objectFields(data)
	if !ok || len(a) != len(b) {
		return false
	}
	keys := map[string]bool{}
	for _, f := range a {
		keys[f.key] = true
	}
	for _, f := range b {
		if !keys[f.key] {
			return false
		}
	}
	return true
}

// leaves maps the path of every scalar in a JSON document to its compact value.
func leaves(data []byte) map[string]string {
	out := map[string]string{}
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return out
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		default:
			b, _ := json.Marshal(x)
			out[path] = string(b)
		}
	}
	walk("$", v)
	return out
}

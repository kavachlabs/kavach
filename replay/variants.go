package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/kavachlabs/kavach/journal"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// MaxCandidates bounds the number of variants generated from one incident.
const MaxCandidates = 64

// vstep is one step of a variant under construction: an input and the reads
// served to whichever input runs in its slot. Gateway and config reads belong
// to the input they were recorded for and travel with it.
type vstep struct {
	input    journal.Record
	reads    []journal.Record
	incident bool
}

type candidate struct {
	mutation string
	steps    []vstep
}

// The classes of mutation a variant belongs to, in Meta.Variant.Kind.
const (
	KindField     = "field"     // one field or array element of the input changed, removed or added
	KindPair      = "pair"      // two fields or elements changed together
	KindMove      = "move"      // the input moved earlier
	KindDrop      = "drop"      // earlier inputs dropped
	KindDuplicate = "duplicate" // an earlier input delivered twice
	KindClock     = "clock"     // every clock read shifted

	// KindRegression marks a variant of an earlier input that resembles the
	// failing one, rather than of the failing input (RegressionVariants).
	KindRegression = "regression"
)

// kinds names the classes candidates returns, in its order.
var kinds = []string{KindField, KindPair, KindMove, KindDrop, KindDuplicate, KindClock}

// Variants derives perturbed versions of the incident recorded in j: the
// failing input with its JSON fields changed, removed or added; the failing
// input moved earlier; earlier inputs dropped or delivered twice; and the clock
// shifted. Each is a journal whose Meta.Variant names the mutation and the
// seq of the input expected to fail. Generation is deterministic.
//
// It returns no variants when j recorded no failure.
func Variants(j *journal.Journal) ([]*journal.Journal, error) {
	snap, env, steps, err := variantSource(j)
	if err != nil {
		return nil, err
	}
	k := -1
	for i, st := range steps {
		if st.marker != nil {
			k = i
			break
		}
	}
	if k < 0 {
		return nil, nil
	}
	return interleave(j.Header.Meta, snap, env, [][][]candidate{candidates(steps, k)}, markerFailure(*steps[k].marker), kinds, MaxCandidates), nil
}

// FeatureVariants derives variants of a journal's feature steps (SPEC.md
// §6.4). For each feature input, in order, it takes the journal up to and
// including that input and perturbs it as Variants perturbs an incident. A
// variant expects no failure: its Meta.Variant.Incident is the seq of the
// feature input, and its Failure is empty. Generation is deterministic.
func FeatureVariants(j *journal.Journal, features map[uint64]bool) ([]*journal.Journal, error) {
	snap, env, steps, err := variantSource(j)
	if err != nil {
		return nil, err
	}
	var each [][][]candidate
	for k, st := range steps {
		if features[st.input.Seq] {
			each = append(each, candidates(steps, k))
		}
	}
	return interleave(j.Header.Meta, snap, env, each, "", kinds, MaxCandidates), nil
}

// variantSource splits j into what variants are built from.
func variantSource(j *journal.Journal) (snap, env *journal.Record, steps []*step, err error) {
	recs := j.Records
	if j.Header.Meta.Start == journal.StartSnapshot {
		if len(recs) == 0 || recs[0].Type != journal.TypeSnapshot {
			return nil, nil, nil, errors.New("kavach: snapshot journal has no snapshot record")
		}
		snap, recs = &recs[0], recs[1:]
	}
	// Variants keep the journal's genesis environment: a replay serves it, and a
	// journal without one is invalid. They never change environment records.
	env = genesisEnv(recs)
	steps, err = splitSteps(recs)
	return snap, env, steps, err
}

// candidates are the mutations of the journal up to and including step k,
// by class.
func candidates(steps []*step, k int) [][]candidate {
	base := make([]vstep, k+1)
	for i, st := range steps[:k+1] {
		base[i] = vstep{input: st.input, reads: st.reads, incident: i == k}
	}
	return [][]candidate{
		fieldMutations(base, k),
		pairMutations(base, k),
		moves(base, k),
		drops(base, k),
		duplicates(base, k),
		clockShifts(base),
	}
}

// interleave builds up to limit variants, taking the i-th candidate of every
// class of every target before any (i+1)-th, so that every target and class is
// varied early. classKinds names the classes, in order, for Variant.Kind.
func interleave(meta journal.Meta, snap, env *journal.Record, targets [][][]candidate, failure string, classKinds []string, limit int) []*journal.Journal {
	var out []*journal.Journal
	for i := 0; len(out) < limit; i++ {
		added := false
		for _, classes := range targets {
			for ci, c := range classes {
				if i < len(c) && len(out) < limit {
					info := journal.Variant{ID: len(out) + 1, Mutation: c[i].mutation, Failure: failure, Kind: classKinds[ci]}
					out = append(out, buildVariant(meta, snap, env, c[i].steps, info))
					added = true
				}
			}
		}
		if !added {
			break
		}
	}
	return out
}

func markerFailure(m journal.Record) string {
	switch m.Kind {
	case journal.MarkerInvariant:
		return "invariant: " + m.Message
	case journal.MarkerCrash:
		return "crash" // SPEC.md §3.2: the whole failure string
	}
	return m.Kind + ": " + m.Message
}

func buildVariant(meta journal.Meta, snap, env *journal.Record, steps []vstep, info journal.Variant) *journal.Journal {
	var recs []journal.Record
	var seq uint64
	add := func(r journal.Record) {
		r.Seq = seq
		seq++
		recs = append(recs, r)
	}
	if snap != nil {
		add(*snap)
	}
	if env != nil {
		add(*env)
	}
	for _, st := range steps {
		if st.incident {
			info.Incident = seq
		}
		add(st.input)
		for _, r := range st.reads {
			add(r)
		}
	}
	meta.Producer = "kavach-go/" + kavach.Version
	meta.Variant = &info
	meta.Run, meta.Segment, meta.CutFrom = "", nil, nil
	return &journal.Journal{Header: journal.Header{Major: journal.Major, Minor: journal.Minor, Meta: meta}, Records: recs}
}

func cloneSteps(s []vstep) []vstep { return append([]vstep(nil), s...) }

// moves runs the incident's input d slots earlier, against less history.
// Clock and random reads stay with their slots, so time still moves forward.
func moves(base []vstep, k int) []candidate {
	var out []candidate
	for d := 1; d <= k; d++ {
		s := cloneSteps(base)
		to := k - d
		inc := s[k]
		copy(s[to+1:k+1], base[to:k])
		s[to] = inc
		for i := range s {
			s[i].reads = append(readsOf(base[i].reads, true), readsOf(s[i].reads, false)...)
		}
		n := "1 input"
		if d > 1 {
			n = fmt.Sprintf("%d inputs", d)
		}
		out = append(out, candidate{fmt.Sprintf("failing input (seq %d) moved %s earlier, before seq %d", base[k].input.Seq, n, base[to].input.Seq), s})
	}
	return out
}

// readsOf returns the clock and random reads of rs, or its other reads.
func readsOf(rs []journal.Record, timeAndRand bool) []journal.Record {
	var out []journal.Record
	for _, r := range rs {
		if (r.Type == journal.TypeClock || r.Type == journal.TypeRand) == timeAndRand {
			out = append(out, r)
		}
	}
	return out
}

// drops removes one earlier step, nearest first, and finally all of them.
func drops(base []vstep, k int) []candidate {
	var out []candidate
	for i := k - 1; i >= 0; i-- {
		s := append(cloneSteps(base[:i]), base[i+1:]...)
		out = append(out, candidate{fmt.Sprintf("input seq %d dropped", base[i].input.Seq), s})
	}
	if k > 1 {
		out = append(out, candidate{fmt.Sprintf("all %d inputs before the failing one dropped", k), cloneSteps(base[k:])})
	}
	return out
}

// duplicates delivers one earlier input twice, as an at-least-once source can.
func duplicates(base []vstep, k int) []candidate {
	var out []candidate
	for i := k - 1; i >= 0; i-- {
		s := append(cloneSteps(base[:i+1]), base[i:]...)
		out = append(out, candidate{fmt.Sprintf("input seq %d delivered twice", base[i].input.Seq), s})
	}
	return out
}

var clockOffsets = []struct {
	d     time.Duration
	label string
}{
	{90 * time.Minute, "+1h30m"},
	{36 * time.Hour, "+36h"},
	{-5*time.Hour - 30*time.Minute, "-5h30m"},
	{366 * 24 * time.Hour, "+366 days"},
}

// clockShifts moves every clock read by a fixed offset.
func clockShifts(base []vstep) []candidate {
	var out []candidate
	for _, off := range clockOffsets {
		s := cloneSteps(base)
		for i := range s {
			reads := make([]journal.Record, len(s[i].reads))
			for j, r := range s[i].reads {
				if r.Type == journal.TypeClock {
					r.UnixNanos += int64(off.d)
				}
				reads[j] = r
			}
			s[i].reads = reads
		}
		out = append(out, candidate{"clock shifted by " + off.label, s})
	}
	return out
}

type field struct {
	key string
	val json.RawMessage
}

type fieldMutation struct {
	desc   string
	fields []field
}

// fieldMutations perturbs the failing input when it is a JSON object: each
// field set to values the same field takes in other inputs and to nearby
// values, removed, and fields seen in other inputs added. An array field is
// also varied one element at a time, with values from the same index in other
// inputs, from other indices of earlier arrays that share the element, and
// nearby values. Mutations are interleaved across fields so that every field
// is varied early.
func fieldMutations(base []vstep, k int) []candidate {
	fields, ok := objectFields(base[k].input.Data)
	if !ok {
		return nil
	}
	seen, recent, order := seenFields(base, k)

	var perKey [][]fieldMutation
	present := map[string]bool{}
	for fi, f := range fields {
		present[f.key] = true
		var ms []fieldMutation
		for _, v := range fieldReplacements(f.val, seen[f.key]) {
			fs := append([]field(nil), fields...)
			fs[fi].val = v
			ms = append(ms, fieldMutation{fmt.Sprintf("%q %s → %s", f.key, f.val, v), fs})
		}
		if len(fields) > 1 {
			fs := append(append([]field(nil), fields[:fi]...), fields[fi+1:]...)
			ms = append(ms, fieldMutation{fmt.Sprintf("%q removed", f.key), fs})
		}
		perKey = append(perKey, ms)

		elems, ok := arrayElements(f.val)
		if !ok {
			continue
		}
		var crossPerElem [][]fieldMutation
		for ei, e := range elems {
			setElem := func(v json.RawMessage) fieldMutation {
				es := append([]json.RawMessage(nil), elems...)
				es[ei] = v
				fs := append([]field(nil), fields...)
				fs[fi].val = encodeArray(es)
				return fieldMutation{fmt.Sprintf("%q[%d] %s → %s", f.key, ei, e, v), fs}
			}
			same, cross, near := elemReplacements(elems, ei, seen[f.key], recent[f.key])
			var ems []fieldMutation
			for _, v := range append(same, near...) {
				ems = append(ems, setElem(v))
			}
			if len(ems) > 0 {
				perKey = append(perKey, ems)
			}
			var cms []fieldMutation
			for _, v := range cross {
				cms = append(cms, setElem(v))
			}
			if len(cms) > 0 {
				crossPerElem = append(crossPerElem, cms)
			}
		}
		if cms := interleaveMutations(crossPerElem); len(cms) > 0 {
			perKey = append(perKey, cms)
		}
	}
	added := 0
	for _, key := range order {
		if present[key] || added == 2 {
			continue
		}
		added++
		v := seen[key][0]
		fs := append(append([]field(nil), fields...), field{key, v})
		perKey = append(perKey, []fieldMutation{{fmt.Sprintf("%q: %s added", key, v), fs}})
	}
	return buildFieldCandidates(base, k, interleaveMutations(perKey))
}

// pairMutations changes two fields, or two elements of an array field, of the
// failing input at once, starting from the trailing fields and arguments.
func pairMutations(base []vstep, k int) []candidate {
	fields, ok := objectFields(base[k].input.Data)
	if !ok {
		return nil
	}
	seen, recent, _ := seenFields(base, k)
	var ms []fieldMutation

	if len(fields) > 1 {
		fvals := make([][]json.RawMessage, len(fields))
		for fi, f := range fields {
			fvals[fi] = fieldReplacements(f.val, seen[f.key])
		}
		for fj := len(fields) - 1; fj >= 1; fj-- {
			for fi := fj - 1; fi >= 0; fi-- {
				vi, vj := fvals[fi], fvals[fj]
				if len(vi) == 0 || len(vj) == 0 {
					continue
				}
				addPair := func(a, b json.RawMessage) {
					fs := append([]field(nil), fields...)
					fs[fi].val, fs[fj].val = a, b
					ms = append(ms, fieldMutation{
						fmt.Sprintf("%q %s → %s, %q %s → %s", fields[fi].key, fields[fi].val, a, fields[fj].key, fields[fj].val, b),
						fs,
					})
				}
				addPair(vi[0], vj[0])
				if len(vi) > 1 {
					addPair(vi[len(vi)-1], vj[0])
				}
			}
		}
	}

	for fi, f := range fields {
		elems, ok := arrayElements(f.val)
		if !ok || len(elems) < 2 {
			continue
		}
		evals := make([][]json.RawMessage, len(elems))
		for ei := range elems {
			same, cross, near := elemReplacements(elems, ei, seen[f.key], recent[f.key])
			evals[ei] = append(append(same, cross...), near...)
		}
		for _, minIdx := range []int{1, 0} {
			for ej := len(elems) - 1; ej >= 1; ej-- {
				for ei := ej - 1; ei >= 0; ei-- {
					if (ei == 0) != (minIdx == 0) {
						continue
					}
					vi, vj := evals[ei], evals[ej]
					if len(vi) == 0 || len(vj) == 0 {
						continue
					}
					addElemPair := func(a, b json.RawMessage) {
						es := append([]json.RawMessage(nil), elems...)
						es[ei], es[ej] = a, b
						fs := append([]field(nil), fields...)
						fs[fi].val = encodeArray(es)
						ms = append(ms, fieldMutation{
							fmt.Sprintf("%q[%d] %s → %s, %q[%d] %s → %s", f.key, ei, elems[ei], a, f.key, ej, elems[ej], b),
							fs,
						})
					}
					addElemPair(vi[0], vj[0])
					if len(vi) > 1 {
						addElemPair(vi[len(vi)-1], vj[0])
					}
					if len(vj) > 1 {
						addElemPair(vi[0], vj[1])
					}
				}
			}
		}
	}
	return buildFieldCandidates(base, k, ms)
}

// seenFields collects the values each key takes in the other inputs in order
// of first appearance (seen) and in reverse step order from k-1 down to 0
// (recent).
func seenFields(base []vstep, k int) (seen, recent map[string][]json.RawMessage, order []string) {
	seen = map[string][]json.RawMessage{}
	recent = map[string][]json.RawMessage{}
	for i, st := range base {
		if i == k {
			continue
		}
		fs, ok := objectFields(st.input.Data)
		if !ok {
			continue
		}
		for _, f := range fs {
			if _, ok := seen[f.key]; !ok {
				order = append(order, f.key)
			}
			if !containsRaw(seen[f.key], f.val) {
				seen[f.key] = append(seen[f.key], f.val)
			}
		}
	}
	for i := k - 1; i >= 0; i-- {
		fs, ok := objectFields(base[i].input.Data)
		if !ok {
			continue
		}
		for _, f := range fs {
			if !containsRaw(recent[f.key], f.val) {
				recent[f.key] = append(recent[f.key], f.val)
			}
		}
	}
	return seen, recent, order
}

func fieldReplacements(val json.RawMessage, others []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for _, o := range others {
		if !bytes.Equal(o, val) && !containsRaw(out, o) {
			out = append(out, o)
			if len(out) == 3 {
				break
			}
		}
	}
	for _, v := range nearby(val) {
		if !bytes.Equal(v, val) && !containsRaw(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// elemReplacements returns replacement values for elems[ei]: up to 3 values at
// the same index in other inputs (same), up to 3 values at other indices of
// earlier arrays that contain elems[ei] at another index (cross), and nearby
// values (near).
func elemReplacements(elems []json.RawMessage, ei int, others, recent []json.RawMessage) (same, cross, near []json.RawMessage) {
	e := elems[ei]
	for _, o := range others {
		if oes, ok := arrayElements(o); ok && ei < len(oes) && !bytes.Equal(oes[ei], e) && !containsRaw(same, oes[ei]) {
			same = append(same, oes[ei])
			if len(same) == 3 {
				break
			}
		}
	}
	for _, o := range recent {
		oes, ok := arrayElements(o)
		if !ok {
			continue
		}
		shifted := false
		for ej, oe := range oes {
			if ej != ei && (ei == 0) == (ej == 0) && bytes.Equal(oe, e) {
				shifted = true
				break
			}
		}
		if !shifted {
			continue
		}
		for ej, oe := range oes {
			if (ei == 0) != (ej == 0) || bytes.Equal(oe, e) || containsRaw(same, oe) || containsRaw(cross, oe) {
				continue
			}
			cross = append(cross, oe)
			if len(cross) == 3 {
				break
			}
		}
		if len(cross) == 3 {
			break
		}
	}
	for _, v := range nearby(e) {
		if !bytes.Equal(v, e) && !containsRaw(same, v) && !containsRaw(cross, v) {
			near = append(near, v)
		}
	}
	return same, cross, near
}

func interleaveMutations(groups [][]fieldMutation) []fieldMutation {
	var out []fieldMutation
	for i := 0; ; i++ {
		more := false
		for _, ms := range groups {
			if i < len(ms) {
				out = append(out, ms[i])
				more = true
			}
		}
		if !more {
			return out
		}
	}
}

func buildFieldCandidates(base []vstep, k int, ms []fieldMutation) []candidate {
	var out []candidate
	for _, m := range ms {
		s := cloneSteps(base)
		s[k].input.Data = encodeObject(m.fields)
		out = append(out, candidate{fmt.Sprintf("failing input (seq %d): %s", base[k].input.Seq, m.desc), s})
	}
	return out
}

// arrayElements splits a compacted JSON array into its compacted elements.
func arrayElements(v json.RawMessage) ([]json.RawMessage, bool) {
	if len(v) == 0 || v[0] != '[' {
		return nil, false
	}
	var es []json.RawMessage
	if json.Unmarshal(v, &es) != nil || len(es) == 0 {
		return nil, false
	}
	return es, true
}

func encodeArray(es []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, e := range es {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(e)
	}
	b.WriteByte(']')
	return b.Bytes()
}

func containsRaw(vs []json.RawMessage, v json.RawMessage) bool {
	for _, x := range vs {
		if bytes.Equal(x, v) {
			return true
		}
	}
	return false
}

// nearby returns values close to v that a handler might treat differently.
func nearby(v json.RawMessage) []json.RawMessage {
	switch {
	case len(v) == 0:
		return nil
	case v[0] == '"':
		var s string
		if json.Unmarshal(v, &s) != nil {
			return nil
		}
		b, _ := json.Marshal(s + "-1")
		return []json.RawMessage{b}
	case string(v) == "true":
		return []json.RawMessage{json.RawMessage("false")}
	case string(v) == "false":
		return []json.RawMessage{json.RawMessage("true")}
	}
	if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
		out := []json.RawMessage{json.RawMessage(strconv.FormatInt(n+1, 10))}
		if n != 0 {
			out = append(out, json.RawMessage(strconv.FormatInt(-n, 10)))
		}
		return out
	}
	if f, err := strconv.ParseFloat(string(v), 64); err == nil {
		return []json.RawMessage{json.RawMessage(strconv.FormatFloat(f*2, 'g', -1, 64))}
	}
	return nil
}

// objectFields splits a JSON object into its fields, in order, with compacted
// values. It reports false for anything else, including duplicate keys.
func objectFields(b []byte) ([]field, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var fs []field
	keys := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := t.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil || keys[key] {
			return nil, false
		}
		var c bytes.Buffer
		if err := json.Compact(&c, raw); err != nil {
			return nil, false
		}
		keys[key] = true
		fs = append(fs, field{key, c.Bytes()})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return fs, len(fs) > 0
}

func encodeObject(fs []field) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fs {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

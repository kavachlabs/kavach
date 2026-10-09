package kavach

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// MaxCandidates bounds the number of variants generated from one incident.
const MaxCandidates = 64

// vstep is one step of a variant under construction: an input and the clock
// and random reads served to whichever input runs in its slot.
type vstep struct {
	input    journal.Record
	reads    []journal.Record
	incident bool
}

type candidate struct {
	mutation string
	steps    []vstep
}

// Variants derives perturbed versions of the incident recorded in j: the
// failing input with its JSON fields changed, removed or added; the failing
// input moved earlier; earlier inputs dropped or delivered twice; and the clock
// shifted. Each is a journal whose Meta.Variant names the mutation and the
// seq of the input expected to fail. Generation is deterministic.
//
// It returns no variants when j recorded no failure.
func Variants(j *journal.Journal) ([]*journal.Journal, error) {
	recs := j.Records
	var snap *journal.Record
	if j.Header.Meta.Start == journal.StartSnapshot {
		if len(recs) == 0 || recs[0].Type != journal.TypeSnapshot {
			return nil, errors.New("kavach: snapshot journal has no snapshot record")
		}
		snap, recs = &recs[0], recs[1:]
	}
	// Variants keep the journal's genesis environment: a replay serves it, and a
	// journal without one is invalid. They never change environment records.
	var env *journal.Record
	for i := range recs {
		if recs[i].Type == journal.TypeEnvironment {
			env = &recs[i]
			break
		}
		if recs[i].Type == journal.TypeInput {
			break
		}
	}
	steps, err := splitSteps(recs)
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
	failure := markerFailure(*steps[k].marker)
	base := make([]vstep, k+1)
	for i, st := range steps[:k+1] {
		base[i] = vstep{input: st.input, reads: st.reads, incident: i == k}
	}

	classes := [][]candidate{
		fieldMutations(base, k),
		moves(base, k),
		drops(base, k),
		duplicates(base, k),
		clockShifts(base),
	}
	var out []*journal.Journal
	for i := 0; len(out) < MaxCandidates; i++ {
		added := false
		for _, c := range classes {
			if i < len(c) && len(out) < MaxCandidates {
				info := journal.Variant{ID: len(out) + 1, Mutation: c[i].mutation, Failure: failure}
				out = append(out, buildVariant(j.Header.Meta, snap, env, c[i].steps, info))
				added = true
			}
		}
		if !added {
			break
		}
	}
	return out, nil
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
	meta.Producer = "kavach-go/" + Version
	meta.Variant = &info
	meta.Run, meta.Segment, meta.CutFrom = "", nil, nil
	return &journal.Journal{Header: journal.Header{Major: journal.Major, Minor: journal.Minor, Meta: meta}, Records: recs}
}

func cloneSteps(s []vstep) []vstep { return append([]vstep(nil), s...) }

// moves runs the incident's input d slots earlier, against less history.
// Reads stay with their slots, so time still moves forward.
func moves(base []vstep, k int) []candidate {
	var out []candidate
	for d := 1; d <= k; d++ {
		s := cloneSteps(base)
		to := k - d
		inc := s[k]
		copy(s[to+1:k+1], base[to:k])
		s[to] = inc
		for i := range s {
			s[i].reads = base[i].reads
		}
		n := "1 input"
		if d > 1 {
			n = fmt.Sprintf("%d inputs", d)
		}
		out = append(out, candidate{fmt.Sprintf("failing input (seq %d) moved %s earlier, before seq %d", base[k].input.Seq, n, base[to].input.Seq), s})
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

// fieldMutations perturbs the failing input when it is a JSON object: each
// field set to values the same field takes in other inputs and to nearby
// values, removed, and fields seen in other inputs added. Mutations are
// interleaved across fields so that every field is varied early.
func fieldMutations(base []vstep, k int) []candidate {
	fields, ok := objectFields(base[k].input.Data)
	if !ok {
		return nil
	}
	// Values each key takes in the other inputs, in order of first appearance.
	seen := map[string][]json.RawMessage{}
	var order []string
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

	type mutation struct {
		desc   string
		fields []field
	}
	var perKey [][]mutation
	present := map[string]bool{}
	for fi, f := range fields {
		present[f.key] = true
		var ms []mutation
		set := func(v json.RawMessage) {
			if bytes.Equal(v, f.val) {
				return
			}
			fs := append([]field(nil), fields...)
			fs[fi].val = v
			ms = append(ms, mutation{fmt.Sprintf("%q %s → %s", f.key, f.val, v), fs})
		}
		others := seen[f.key]
		for i := 0; i < len(others) && i < 3; i++ {
			set(others[i])
		}
		for _, v := range nearby(f.val) {
			if !containsRaw(others, v) {
				set(v)
			}
		}
		if len(fields) > 1 {
			fs := append(append([]field(nil), fields[:fi]...), fields[fi+1:]...)
			ms = append(ms, mutation{fmt.Sprintf("%q removed", f.key), fs})
		}
		perKey = append(perKey, ms)
	}
	added := 0
	for _, key := range order {
		if present[key] || added == 2 {
			continue
		}
		added++
		v := seen[key][0]
		fs := append(append([]field(nil), fields...), field{key, v})
		perKey = append(perKey, []mutation{{fmt.Sprintf("%q: %s added", key, v), fs}})
	}

	var out []candidate
	for i := 0; ; i++ {
		more := false
		for _, ms := range perKey {
			if i >= len(ms) {
				continue
			}
			more = true
			s := cloneSteps(base)
			s[k].input.Data = encodeObject(ms[i].fields)
			out = append(out, candidate{fmt.Sprintf("failing input (seq %d): %s", base[k].input.Seq, ms[i].desc), s})
		}
		if !more {
			return out
		}
	}
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

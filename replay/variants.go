package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kavachlabs/kavach/journal"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// MaxCandidates bounds the number of variants generated from one incident.
const MaxCandidates = 64

const maxMutationDepth = 4

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

type mutationOpts struct {
	signPreserving bool
}

const (
	KindField        = "field"         // one field or array element of the input changed, removed or added
	KindPair         = "pair"          // two fields or elements changed together
	KindMove         = "move"          // the input moved earlier
	KindDrop         = "drop"          // earlier inputs dropped
	KindDuplicate    = "duplicate"     // an earlier input delivered twice
	KindClock        = "clock"         // every clock read shifted
	KindGatewayFault = "gateway_fault" // transient driver/gateway error injected + redelivery

	// KindRegression marks a variant of an earlier input that resembles the
	// failing one, rather than of the failing input (RegressionVariants).
	KindRegression = "regression"
)

var kinds = []string{KindField, KindPair, KindMove, KindDrop, KindDuplicate, KindClock, KindGatewayFault}

var metadataKeys = map[string]bool{
	"email":          true,
	"customer_email": true,
	"customer_name":  true,
	"full_name":      true,
	"first_name":     true,
	"last_name":      true,
	"phone":          true,
	"phone_number":   true,
	"ssn":            true,
	"iban":           true,
	"card_pan":       true,
	"pan":            true,
	"ip":             true,
	"ip_address":     true,
	"trace_id":       true,
	"span_id":        true,
	"request_id":     true,
	"correlation_id": true,
	"user_agent":     true,
	"api_key":        true,
	"api_token":      true,
	"jwt":            true,
	"authorization":  true,
	"password":       true,
	"secret":         true,
	"client_retry":   true,
	"request_seq":    true,
	"cost_center":    true,
}

var metadataSuffixes = []string{
	"_email",
	"_phone",
	"_phone_number",
	"_ssn",
	"_iban",
	"_card_pan",
	"_pan",
	"_ip",
	"_ip_address",
	"_trace_id",
	"_span_id",
	"_request_id",
	"_correlation_id",
	"_user_agent",
	"_api_key",
	"_api_token",
	"_jwt",
	"_authorization",
	"_password",
	"_secret",
	"_client_retry",
	"_request_seq",
	"_cost_center",
}

// IsMetadataKey reports whether key (a field name or dot/index path) names a
// non-semantic PII, tracing, or telemetry metadata leaf rather than domain data.
func IsMetadataKey(key string) bool {
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		key = key[i+1:]
	}
	if i := strings.IndexByte(key, '['); i >= 0 {
		key = key[:i]
	}
	if key == "" {
		return false
	}
	norm := normalizeKeySegment(key)
	if metadataKeys[norm] {
		return true
	}
	if trimmed, ok := strings.CutPrefix(norm, "x_"); ok && metadataKeys[trimmed] {
		return true
	}
	for _, suf := range metadataSuffixes {
		if strings.HasSuffix(norm, suf) {
			return true
		}
	}
	return false
}

func normalizeKeySegment(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	runes := []rune(s)
	lastUnderscore := true
	for i, r := range runes {
		if r == '-' || r == ' ' || r == '_' {
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
			continue
		}
		if unicode.IsUpper(r) {
			if i > 0 && !lastUnderscore {
				prev := runes[i-1]
				nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
			lastUnderscore = false
			continue
		}
		b.WriteRune(unicode.ToLower(r))
		lastUnderscore = false
	}
	return strings.Trim(b.String(), "_")
}

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

func candidates(steps []*step, k int) [][]candidate {
	return candidatesWith(steps, k, mutationOpts{})
}

func candidatesWith(steps []*step, k int, opts mutationOpts) [][]candidate {
	base := make([]vstep, k+1)
	for i, st := range steps[:k+1] {
		base[i] = vstep{input: st.input, reads: st.reads, incident: i == k}
	}
	return [][]candidate{
		fieldMutationsWith(base, k, opts),
		pairMutationsWith(base, k, opts),
		moves(base, k),
		drops(base, k),
		duplicates(base, k),
		clockShifts(base, k),
		gatewayFaults(base, steps, k),
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

func readsOf(rs []journal.Record, timeAndRand bool) []journal.Record {
	var out []journal.Record
	for _, r := range rs {
		if (r.Type == journal.TypeClock || r.Type == journal.TypeRand) == timeAndRand {
			out = append(out, r)
		}
	}
	return out
}

var commonOpVerbs = map[string]bool{
	"true": true, "false": true, "null": true,
	"get": true, "set": true, "put": true, "del": true, "delete": true,
	"post": true, "patch": true, "head": true, "options": true, "ping": true,
	"create": true, "update": true, "upsert": true, "insert": true, "select": true,
	"list": true, "read": true, "write": true, "transfer": true, "debit": true,
	"credit": true, "refund": true, "capture": true, "void": true, "cancel": true,
	"charge": true, "check": true, "verify": true, "auth": true, "authorize": true,
	"confirm": true, "complete": true, "fail": true, "retry": true, "ack": true,
	"nack": true, "publish": true, "consume": true, "emit": true, "send": true,
	"recv": true, "open": true, "close": true, "init": true, "start": true,
	"stop": true, "run": true, "exec": true, "eval": true, "bitpos": true,
	"bitcount": true, "setbit": true, "zadd": true, "zcard": true, "bzmpop": true,
	"incr": true, "decr": true, "add": true, "sub": true, "mul": true, "div": true,
	"none": true, "yes": true, "not": true, "err": true, "error": true,
	"info": true, "warn": true, "debug": true, "status": true, "pending": true,
	"active": true, "failed": true, "done": true, "success": true, "completed": true,
	"cancelled": true, "canceled": true,
}

func isSymbolicHandle(s string) bool {
	if len(s) < 3 || commonOpVerbs[strings.ToLower(s)] || isoCurrencies[s] {
		return false
	}
	if _, _, ok := parseRFC3339(s); ok {
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return false
	}
	return true
}

func extractHandles(data []byte) []string {
	seen := map[string]bool{}
	var out []string
	addToken := func(s string) {
		if !isSymbolicHandle(s) {
			return
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if root, ok := parseJSONNode(data); ok {
		var walk func(n *jsonNode)
		walk = func(n *jsonNode) {
			switch n.kind {
			case nodeObject:
				for _, f := range n.fields {
					walk(f.val)
				}
			case nodeArray:
				for _, e := range n.elems {
					walk(e)
				}
			case nodePrimitive:
				if len(n.raw) > 0 && n.raw[0] == '"' {
					var s string
					if json.Unmarshal(n.raw, &s) == nil {
						addToken(s)
					}
				}
			}
		}
		walk(root)
		return out
	}
	for _, tok := range splitPlainTokens(data) {
		addToken(tok)
	}
	return out
}

// collectSymbolicHandles identifies:
//   - crossHandles: symbolic handles in base[k].input.Data that also appeared in
//     at least one earlier step's input or reads.
//   - intraShared: symbolic handles appearing >= 2 times across distinct leaves
//     inside base[k].input.Data (full depth walk).
func collectSymbolicHandles(base []vstep, k int, root *jsonNode) (crossHandles, intraShared map[string]bool) {
	crossHandles = map[string]bool{}
	intraShared = map[string]bool{}

	counts := map[string]int{}
	var walk func(n *jsonNode)
	walk = func(n *jsonNode) {
		switch n.kind {
		case nodeObject:
			for _, f := range n.fields {
				walk(f.val)
			}
		case nodeArray:
			for _, e := range n.elems {
				walk(e)
			}
		case nodePrimitive:
			if len(n.raw) > 0 && n.raw[0] == '"' {
				var s string
				if json.Unmarshal(n.raw, &s) == nil && isSymbolicHandle(s) {
					counts[s]++
				}
			}
		}
	}
	walk(root)

	var candidates []string
	for s, cnt := range counts {
		candidates = append(candidates, s)
		if cnt >= 2 {
			intraShared[s] = true
		}
	}
	if len(candidates) > 0 && k > 0 {
		for i := 0; i < k; i++ {
			toks := collectStepTokens(base[i])
			for _, h := range candidates {
				if toks[h] {
					crossHandles[h] = true
				}
			}
		}
	}
	return crossHandles, intraShared
}

func jsonStringValue(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func collectStepTokens(st vstep) map[string]bool {
	toks := map[string]bool{}
	addData := func(b []byte) {
		if len(b) == 0 {
			return
		}
		if root, ok := parseJSONNode(b); ok {
			var walk func(n *jsonNode)
			walk = func(n *jsonNode) {
				switch n.kind {
				case nodeObject:
					for _, f := range n.fields {
						walk(f.val)
					}
				case nodeArray:
					for _, e := range n.elems {
						walk(e)
					}
				case nodePrimitive:
					if len(n.raw) > 0 && n.raw[0] == '"' {
						var s string
						if json.Unmarshal(n.raw, &s) == nil && len(s) >= 3 {
							toks[s] = true
						}
					}
				}
			}
			walk(root)
			return
		}
		for _, tok := range splitPlainTokens(b) {
			if len(tok) >= 3 {
				toks[tok] = true
			}
		}
	}
	addData(st.input.Data)
	for _, r := range st.reads {
		addData(r.Data)
		addData(r.Request)
		addData(r.Response)
		addData(r.Value)
	}
	return toks
}

func splitPlainTokens(b []byte) []string {
	return strings.FieldsFunc(string(b), func(r rune) bool {
		switch r {
		case ':', ',', ';', '=', '"', '\'', '(', ')', '[', ']', '{', '}':
			return true
		}
		return unicode.IsSpace(r)
	})
}

func drops(base []vstep, k int) []candidate {
	handles := extractHandles(base[k].input.Data)
	var nonReferenced, referenced []candidate
	for i := k - 1; i >= 0; i-- {
		s := append(cloneSteps(base[:i]), base[i+1:]...)
		c := candidate{fmt.Sprintf("input seq %d dropped", base[i].input.Seq), s}
		if len(handles) > 0 && stepMentionsAny(base[i], handles) {
			referenced = append(referenced, c)
		} else {
			nonReferenced = append(nonReferenced, c)
		}
	}
	out := append(nonReferenced, referenced...)
	if k > 1 {
		out = append(out, candidate{fmt.Sprintf("all %d inputs before the failing one dropped", k), cloneSteps(base[k:])})
	}
	return out
}

func stepMentionsAny(st vstep, handles []string) bool {
	toks := collectStepTokens(st)
	for _, h := range handles {
		if toks[h] {
			return true
		}
	}
	return false
}

func maxClockInSteps(steps []vstep) (int64, bool) {
	var maxNanos int64
	has := false
	for _, st := range steps {
		for _, r := range st.reads {
			if r.Type == journal.TypeClock {
				if !has || r.UnixNanos > maxNanos {
					maxNanos = r.UnixNanos
					has = true
				}
			}
		}
	}
	return maxNanos, has
}

func clampClockReads(rs []journal.Record, minNanos int64, hasMin bool) []journal.Record {
	if !hasMin {
		return rs
	}
	cloned := append([]journal.Record(nil), rs...)
	for i := range cloned {
		if cloned[i].Type == journal.TypeClock && cloned[i].UnixNanos < minNanos {
			cloned[i].UnixNanos = minNanos
		}
	}
	return cloned
}

func duplicates(base []vstep, k int) []candidate {
	var immediate []candidate
	for i := k - 1; i >= 0; i-- {
		s := append(cloneSteps(base[:i+1]), base[i:]...)
		immediate = append(immediate, candidate{fmt.Sprintf("input seq %d delivered twice", base[i].input.Seq), s})
	}

	// Failing-step redelivery: deliver s[k] twice in a row with the second copy
	// marked as the incident to test idempotency on redelivery of the failing input.
	failing := make([]vstep, 0, len(base)+1)
	failing = append(failing, base[:k]...)
	first := base[k]
	first.incident = false
	second := base[k]
	second.incident = true
	failing = append(failing, first, second)
	failingCand := candidate{fmt.Sprintf("failing input (seq %d) delivered twice", base[k].input.Seq), failing}

	// Delayed redelivery: for prior steps i < k-1, redeliver s[i] right before s[k].
	maxPriorClock, hasPriorClock := maxClockInSteps(base[:k])
	var delayed []candidate
	for i := k - 2; i >= 0; i-- {
		s := make([]vstep, 0, len(base)+1)
		s = append(s, base[:k]...)
		dup := base[i]
		dup.incident = false
		dup.reads = clampClockReads(dup.reads, maxPriorClock, hasPriorClock)
		s = append(s, dup, base[k])
		delayed = append(delayed, candidate{fmt.Sprintf("input seq %d delivered twice (delayed before seq %d)", base[i].input.Seq, base[k].input.Seq), s})
	}

	var out []candidate
	if len(immediate) > 0 {
		out = append(out, immediate[0])
		out = append(out, failingCand)
		for idx := 0; idx < len(delayed) || idx+1 < len(immediate); idx++ {
			if idx < len(delayed) {
				out = append(out, delayed[idx])
			}
			if idx+1 < len(immediate) {
				out = append(out, immediate[idx+1])
			}
		}
	} else {
		out = append(out, failingCand)
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

var relativeClockSkews = []struct {
	d     time.Duration
	label string
}{
	{time.Millisecond, "+1ms"},
	{time.Second, "+1s"},
	{time.Minute, "+1m"},
	{time.Hour, "+1h"},
}

func hasClockReads(rs []journal.Record) bool {
	for _, r := range rs {
		if r.Type == journal.TypeClock {
			return true
		}
	}
	return false
}

func clockShifts(base []vstep, k int) []candidate {
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
	if k > 0 && hasClockReads(base[k].reads) {
		maxPriorClock, hasPriorClock := maxClockInSteps(base[:k])
		for _, skew := range relativeClockSkews {
			s := cloneSteps(base)
			reads := make([]journal.Record, len(s[k].reads))
			for j, r := range s[k].reads {
				if r.Type == journal.TypeClock {
					r.UnixNanos += int64(skew.d)
					if hasPriorClock && r.UnixNanos < maxPriorClock {
						r.UnixNanos = maxPriorClock
					}
				}
				reads[j] = r
			}
			s[k].reads = reads
			out = append(out, candidate{"failing step clock skewed by " + skew.label, s})
		}
	}
	return out
}

type field struct {
	key string
	val json.RawMessage
}

type fieldMutation struct {
	desc string
	data []byte
}

type pathSeg struct {
	isIndex bool
	key     string
	index   int
}

type jsonNodeKind int

const (
	nodePrimitive jsonNodeKind = iota
	nodeObject
	nodeArray
)

type jsonField struct {
	key string
	val *jsonNode
}

type jsonNode struct {
	kind   jsonNodeKind
	raw    json.RawMessage
	fields []jsonField
	elems  []*jsonNode
}

func (n *jsonNode) encode() []byte {
	var b bytes.Buffer
	n.writeTo(&b)
	return b.Bytes()
}

func (n *jsonNode) writeTo(b *bytes.Buffer) {
	switch n.kind {
	case nodePrimitive:
		b.Write(n.raw)
	case nodeObject:
		b.WriteByte('{')
		for i, f := range n.fields {
			if i > 0 {
				b.WriteByte(',')
			}
			k := marshalJSONNoEscape(f.key)
			b.Write(k)
			b.WriteByte(':')
			f.val.writeTo(b)
		}
		b.WriteByte('}')
	case nodeArray:
		b.WriteByte('[')
		for i, e := range n.elems {
			if i > 0 {
				b.WriteByte(',')
			}
			e.writeTo(b)
		}
		b.WriteByte(']')
	}
}

func (n *jsonNode) shallowClone() *jsonNode {
	c := *n
	if n.fields != nil {
		c.fields = append([]jsonField(nil), n.fields...)
	}
	if n.elems != nil {
		c.elems = append([]*jsonNode(nil), n.elems...)
	}
	return &c
}

func rawNode(raw json.RawMessage) *jsonNode {
	return &jsonNode{kind: nodePrimitive, raw: raw}
}

func marshalJSONNoEscape(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func setAtPath(root *jsonNode, segs []pathSeg, val *jsonNode) *jsonNode {
	if len(segs) == 0 {
		return val
	}
	c := root.shallowClone()
	seg := segs[0]
	if !seg.isIndex {
		for i, f := range c.fields {
			if f.key == seg.key {
				c.fields[i].val = setAtPath(f.val, segs[1:], val)
				break
			}
		}
	} else if seg.index >= 0 && seg.index < len(c.elems) {
		c.elems[seg.index] = setAtPath(c.elems[seg.index], segs[1:], val)
	}
	return c
}

func removeAtPath(root *jsonNode, parentSegs []pathSeg, key string) *jsonNode {
	if len(parentSegs) == 0 {
		c := root.shallowClone()
		for i, f := range c.fields {
			if f.key == key {
				c.fields = append(c.fields[:i], c.fields[i+1:]...)
				break
			}
		}
		return c
	}
	c := root.shallowClone()
	seg := parentSegs[0]
	if !seg.isIndex {
		for i, f := range c.fields {
			if f.key == seg.key {
				c.fields[i].val = removeAtPath(f.val, parentSegs[1:], key)
				break
			}
		}
	} else if seg.index >= 0 && seg.index < len(c.elems) {
		c.elems[seg.index] = removeAtPath(c.elems[seg.index], parentSegs[1:], key)
	}
	return c
}

func addFieldAtPath(root *jsonNode, parentSegs []pathSeg, key string, val *jsonNode) *jsonNode {
	if len(parentSegs) == 0 {
		c := root.shallowClone()
		c.fields = append(c.fields, jsonField{key: key, val: val})
		return c
	}
	c := root.shallowClone()
	seg := parentSegs[0]
	if !seg.isIndex {
		for i, f := range c.fields {
			if f.key == seg.key {
				c.fields[i].val = addFieldAtPath(f.val, parentSegs[1:], key, val)
				break
			}
		}
	} else if seg.index >= 0 && seg.index < len(c.elems) {
		c.elems[seg.index] = addFieldAtPath(c.elems[seg.index], parentSegs[1:], key, val)
	}
	return c
}

func parseJSONNode(b []byte) (*jsonNode, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n, ok := decodeJSONNode(dec)
	if !ok {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return n, true
}

func parseRootObject(b []byte) (*jsonNode, bool) {
	root, ok := parseJSONNode(b)
	if !ok || root.kind != nodeObject || len(root.fields) == 0 {
		return nil, false
	}
	return root, true
}

func decodeJSONNode(dec *json.Decoder) (*jsonNode, bool) {
	t, err := dec.Token()
	if err != nil {
		return nil, false
	}
	switch v := t.(type) {
	case json.Delim:
		switch v {
		case '{':
			var fs []jsonField
			keys := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, false
				}
				key, ok := kt.(string)
				if !ok || keys[key] {
					return nil, false
				}
				val, ok := decodeJSONNode(dec)
				if !ok {
					return nil, false
				}
				keys[key] = true
				fs = append(fs, jsonField{key: key, val: val})
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				return nil, false
			}
			return &jsonNode{kind: nodeObject, fields: fs}, true
		case '[':
			var es []*jsonNode
			for dec.More() {
				e, ok := decodeJSONNode(dec)
				if !ok {
					return nil, false
				}
				es = append(es, e)
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				return nil, false
			}
			return &jsonNode{kind: nodeArray, elems: es}, true
		default:
			return nil, false
		}
	case string:
		return rawNode(marshalJSONNoEscape(v)), true
	case json.Number:
		return rawNode(json.RawMessage(v)), true
	case bool:
		if v {
			return rawNode(json.RawMessage("true")), true
		}
		return rawNode(json.RawMessage("false")), true
	case nil:
		return rawNode(json.RawMessage("null")), true
	default:
		return nil, false
	}
}

func isContainerArray(n *jsonNode) bool {
	if n.kind != nodeArray || len(n.elems) == 0 {
		return false
	}
	for _, e := range n.elems {
		if (e.kind == nodeObject && len(e.fields) > 0) || (e.kind == nodeArray && len(e.elems) > 0) {
			return true
		}
	}
	return false
}

func joinFieldPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func appendSeg(segs []pathSeg, seg pathSeg) []pathSeg {
	out := make([]pathSeg, len(segs)+1)
	copy(out, segs)
	out[len(segs)] = seg
	return out
}

func containerHasSemanticLeaf(n *jsonNode, path string, depth int) bool {
	if IsMetadataKey(path) {
		return false
	}
	switch n.kind {
	case nodeObject:
		if depth >= maxMutationDepth || len(n.fields) == 0 {
			return true
		}
		for _, f := range n.fields {
			if containerHasSemanticLeaf(f.val, joinFieldPath(path, f.key), depth+1) {
				return true
			}
		}
		return false
	case nodeArray:
		if depth >= maxMutationDepth || !isContainerArray(n) {
			return true
		}
		for i, e := range n.elems {
			if containerHasSemanticLeaf(e, fmt.Sprintf("%s[%d]", path, i), depth+1) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func fieldMutationsWith(base []vstep, k int, opts mutationOpts) []candidate {
	root, ok := parseRootObject(base[k].input.Data)
	if !ok {
		return nil
	}
	seen, recent, childOrder := seenFields(base, k)
	crossHandles, intraShared := collectSymbolicHandles(base, k, root)

	var semanticPerKey [][]fieldMutation
	var metaPerKey [][]fieldMutation

	addPrimitiveArray := func(n *jsonNode, segs, parentSegs []pathSeg, fieldKey string, parentLen int, path string) {
		rawVal := n.encode()
		if IsMetadataKey(path) {
			var mms []fieldMutation
			for _, v := range seenReplacementsOnly(rawVal, seen[path]) {
				mms = append(mms, fieldMutation{
					desc: fmt.Sprintf("%q %s → %s", path, rawVal, v),
					data: setAtPath(root, segs, rawNode(v)).encode(),
				})
			}
			if len(mms) > 0 {
				metaPerKey = append(metaPerKey, mms)
			}
			return
		}
		var ms []fieldMutation
		for _, v := range fieldReplacementsWith(rawVal, seen[path], false, opts) {
			ms = append(ms, fieldMutation{
				desc: fmt.Sprintf("%q %s → %s", path, rawVal, v),
				data: setAtPath(root, segs, rawNode(v)).encode(),
			})
		}
		if parentLen > 1 {
			ms = append(ms, fieldMutation{
				desc: fmt.Sprintf("%q removed", path),
				data: removeAtPath(root, parentSegs, fieldKey).encode(),
			})
		}
		semanticPerKey = append(semanticPerKey, ms)

		elems := make([]json.RawMessage, len(n.elems))
		for ei, e := range n.elems {
			elems[ei] = e.encode()
		}
		if len(elems) == 0 {
			return
		}
		var crossPerElem [][]fieldMutation
		for ei, e := range elems {
			if s, ok := jsonStringValue(e); ok && intraShared[s] {
				continue
			}
			setElem := func(v json.RawMessage) fieldMutation {
				es := append([]json.RawMessage(nil), elems...)
				es[ei] = v
				return fieldMutation{
					desc: fmt.Sprintf("%q[%d] %s → %s", path, ei, e, v),
					data: setAtPath(root, segs, rawNode(encodeArray(es))).encode(),
				}
			}
			skipNear := false
			if s, ok := jsonStringValue(e); ok && crossHandles[s] {
				skipNear = true
			}
			same, cross, near := elemReplacementsWith(elems, ei, seen[path], recent[path], skipNear, opts)
			var ems []fieldMutation
			for _, v := range append(same, near...) {
				ems = append(ems, setElem(v))
			}
			if len(ems) > 0 {
				semanticPerKey = append(semanticPerKey, ems)
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
			semanticPerKey = append(semanticPerKey, cms)
		}
	}

	var walk func(n *jsonNode, segs, parentSegs []pathSeg, fieldKey string, parentLen int, path string, depth int)
	walk = func(n *jsonNode, segs, parentSegs []pathSeg, fieldKey string, parentLen int, path string, depth int) {
		switch {
		case n.kind == nodeObject && len(n.fields) > 0:
			if depth >= maxMutationDepth {
				return
			}
			present := map[string]bool{}
			for _, f := range n.fields {
				present[f.key] = true
				childSegs := appendSeg(segs, pathSeg{key: f.key})
				childPath := joinFieldPath(path, f.key)
				walk(f.val, childSegs, segs, f.key, len(n.fields), childPath, depth+1)
			}
			added := 0
			for _, key := range childOrder[path] {
				if present[key] || added == 2 {
					continue
				}
				fullPath := joinFieldPath(path, key)
				if IsMetadataKey(fullPath) {
					continue
				}
				added++
				v := seen[fullPath][0]
				semanticPerKey = append(semanticPerKey, []fieldMutation{{
					desc: fmt.Sprintf("%q: %s added", fullPath, v),
					data: addFieldAtPath(root, segs, key, rawNode(v)).encode(),
				}})
			}
			if depth > 0 && parentLen > 1 && containerHasSemanticLeaf(n, path, depth) {
				semanticPerKey = append(semanticPerKey, []fieldMutation{{
					desc: fmt.Sprintf("%q removed", path),
					data: removeAtPath(root, parentSegs, fieldKey).encode(),
				}})
			}
		case n.kind == nodeArray && isContainerArray(n):
			if depth >= maxMutationDepth {
				return
			}
			for ei, e := range n.elems {
				elemSegs := appendSeg(segs, pathSeg{isIndex: true, index: ei})
				elemPath := fmt.Sprintf("%s[%d]", path, ei)
				walk(e, elemSegs, segs, "", 0, elemPath, depth+1)
			}
			if depth > 0 && parentLen > 1 && containerHasSemanticLeaf(n, path, depth) {
				semanticPerKey = append(semanticPerKey, []fieldMutation{{
					desc: fmt.Sprintf("%q removed", path),
					data: removeAtPath(root, parentSegs, fieldKey).encode(),
				}})
			}
		case n.kind == nodeArray:
			addPrimitiveArray(n, segs, parentSegs, fieldKey, parentLen, path)
		default:
			rawVal := n.encode()
			if IsMetadataKey(path) {
				var mms []fieldMutation
				for _, v := range seenReplacementsOnly(rawVal, seen[path]) {
					mms = append(mms, fieldMutation{
						desc: fmt.Sprintf("%q %s → %s", path, rawVal, v),
						data: setAtPath(root, segs, rawNode(v)).encode(),
					})
				}
				if len(mms) > 0 {
					metaPerKey = append(metaPerKey, mms)
				}
				return
			}
			if s, ok := jsonStringValue(rawVal); ok && intraShared[s] {
				return
			}
			skipNear := false
			if s, ok := jsonStringValue(rawVal); ok && crossHandles[s] {
				skipNear = true
			}
			var ms []fieldMutation
			for _, v := range fieldReplacementsWith(rawVal, seen[path], skipNear, opts) {
				ms = append(ms, fieldMutation{
					desc: fmt.Sprintf("%q %s → %s", path, rawVal, v),
					data: setAtPath(root, segs, rawNode(v)).encode(),
				})
			}
			if parentLen > 1 {
				ms = append(ms, fieldMutation{
					desc: fmt.Sprintf("%q removed", path),
					data: removeAtPath(root, parentSegs, fieldKey).encode(),
				})
			}
			if len(ms) > 0 {
				semanticPerKey = append(semanticPerKey, ms)
			}
		}
	}

	walk(root, nil, nil, "", 0, "", 0)

	if sem := interleaveMutations(semanticPerKey); len(sem) > 0 {
		return buildFieldCandidates(base, k, sem)
	}
	return buildFieldCandidates(base, k, interleaveMutations(metaPerKey))
}

func pairMutationsWith(base []vstep, k int, opts mutationOpts) []candidate {
	root, ok := parseRootObject(base[k].input.Data)
	if !ok {
		return nil
	}
	seen, recent, _ := seenFields(base, k)
	crossHandles, intraShared := collectSymbolicHandles(base, k, root)

	type leafTarget struct {
		segs []pathSeg
		path string
		val  json.RawMessage
		reps []json.RawMessage
	}
	type primArrayTarget struct {
		segs  []pathSeg
		path  string
		elems []json.RawMessage
	}

	var leaves []leafTarget
	var arrays []primArrayTarget

	var collect func(n *jsonNode, segs []pathSeg, path string, depth int)
	collect = func(n *jsonNode, segs []pathSeg, path string, depth int) {
		switch {
		case n.kind == nodeObject && len(n.fields) > 0:
			if depth >= maxMutationDepth {
				return
			}
			for _, f := range n.fields {
				collect(f.val, appendSeg(segs, pathSeg{key: f.key}), joinFieldPath(path, f.key), depth+1)
			}
		case n.kind == nodeArray && isContainerArray(n):
			if depth >= maxMutationDepth {
				return
			}
			for ei, e := range n.elems {
				collect(e, appendSeg(segs, pathSeg{isIndex: true, index: ei}), fmt.Sprintf("%s[%d]", path, ei), depth+1)
			}
		case n.kind == nodeArray:
			if IsMetadataKey(path) {
				return
			}
			rawVal := n.encode()
			leaves = append(leaves, leafTarget{
				segs: segs,
				path: path,
				val:  rawVal,
				reps: fieldReplacementsWith(rawVal, seen[path], false, opts),
			})
			if len(n.elems) >= 2 {
				elems := make([]json.RawMessage, len(n.elems))
				for ei, e := range n.elems {
					elems[ei] = e.encode()
				}
				arrays = append(arrays, primArrayTarget{segs: segs, path: path, elems: elems})
			}
		default:
			if IsMetadataKey(path) {
				return
			}
			rawVal := n.encode()
			skipNear := false
			if s, ok := jsonStringValue(rawVal); ok {
				if intraShared[s] {
					return
				}
				if crossHandles[s] {
					skipNear = true
				}
			}
			leaves = append(leaves, leafTarget{
				segs: segs,
				path: path,
				val:  rawVal,
				reps: fieldReplacementsWith(rawVal, seen[path], skipNear, opts),
			})
		}
	}
	collect(root, nil, "", 0)

	var ms []fieldMutation
	if len(leaves) > 1 {
		for fj := len(leaves) - 1; fj >= 1; fj-- {
			for fi := fj - 1; fi >= 0; fi-- {
				vi, vj := leaves[fi].reps, leaves[fj].reps
				if len(vi) == 0 || len(vj) == 0 {
					continue
				}
				addPair := func(a, b json.RawMessage) {
					mutRoot := setAtPath(root, leaves[fi].segs, rawNode(a))
					mutRoot = setAtPath(mutRoot, leaves[fj].segs, rawNode(b))
					ms = append(ms, fieldMutation{
						desc: fmt.Sprintf("%q %s → %s, %q %s → %s", leaves[fi].path, leaves[fi].val, a, leaves[fj].path, leaves[fj].val, b),
						data: mutRoot.encode(),
					})
				}
				addPair(vi[0], vj[0])
				if len(vi) > 1 {
					addPair(vi[1], vj[0])
				}
			}
		}
	}

	for _, arr := range arrays {
		elems := arr.elems
		evals := make([][]json.RawMessage, len(elems))
		for ei := range elems {
			if s, ok := jsonStringValue(elems[ei]); ok && intraShared[s] {
				continue
			}
			skipNear := false
			if s, ok := jsonStringValue(elems[ei]); ok && crossHandles[s] {
				skipNear = true
			}
			same, cross, near := elemReplacementsWith(elems, ei, seen[arr.path], recent[arr.path], skipNear, opts)
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
						mutRoot := setAtPath(root, arr.segs, rawNode(encodeArray(es)))
						ms = append(ms, fieldMutation{
							desc: fmt.Sprintf("%q[%d] %s → %s, %q[%d] %s → %s", arr.path, ei, elems[ei], a, arr.path, ej, elems[ej], b),
							data: mutRoot.encode(),
						})
					}
					addElemPair(vi[0], vj[0])
					if len(vi) > 1 {
						addElemPair(vi[1], vj[0])
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

// seenFields collects the values each path takes in the other inputs in order
// of first appearance (seen), in reverse step order from k-1 down to 0
// (recent), and the child key order per parent object path (childOrder).
func seenFields(base []vstep, k int) (seen, recent map[string][]json.RawMessage, childOrder map[string][]string) {
	seen = map[string][]json.RawMessage{}
	recent = map[string][]json.RawMessage{}
	childOrder = map[string][]string{}
	seenChild := map[string]map[string]bool{}

	var walkSeen func(n *jsonNode, parentPath string, depth int, onLeaf func(parent, key, path string, raw json.RawMessage))
	walkSeen = func(n *jsonNode, parentPath string, depth int, onLeaf func(parent, key, path string, raw json.RawMessage)) {
		if depth >= maxMutationDepth {
			return
		}
		switch n.kind {
		case nodeObject:
			for _, f := range n.fields {
				p := joinFieldPath(parentPath, f.key)
				onLeaf(parentPath, f.key, p, f.val.encode())
				if depth+1 < maxMutationDepth {
					if f.val.kind == nodeObject && len(f.val.fields) > 0 {
						walkSeen(f.val, p, depth+1, onLeaf)
					} else if f.val.kind == nodeArray && isContainerArray(f.val) {
						walkSeen(f.val, p, depth+1, onLeaf)
					}
				}
			}
		case nodeArray:
			for ei, e := range n.elems {
				ep := fmt.Sprintf("%s[%d]", parentPath, ei)
				if depth+1 < maxMutationDepth && ((e.kind == nodeObject && len(e.fields) > 0) || (e.kind == nodeArray && isContainerArray(e))) {
					walkSeen(e, ep, depth+1, onLeaf)
				} else {
					onLeaf("", "", ep, e.encode())
				}
			}
		}
	}

	for i, st := range base {
		if i == k {
			continue
		}
		root, ok := parseRootObject(st.input.Data)
		if !ok {
			continue
		}
		walkSeen(root, "", 0, func(parent, key, path string, raw json.RawMessage) {
			if key != "" {
				if seenChild[parent] == nil {
					seenChild[parent] = map[string]bool{}
				}
				if !seenChild[parent][key] {
					seenChild[parent][key] = true
					childOrder[parent] = append(childOrder[parent], key)
				}
			}
			if !containsRaw(seen[path], raw) {
				seen[path] = append(seen[path], raw)
			}
		})
	}
	for i := k - 1; i >= 0; i-- {
		root, ok := parseRootObject(base[i].input.Data)
		if !ok {
			continue
		}
		walkSeen(root, "", 0, func(_, _, path string, raw json.RawMessage) {
			if !containsRaw(recent[path], raw) {
				recent[path] = append(recent[path], raw)
			}
		})
	}
	return seen, recent, childOrder
}

func seenReplacementsOnly(val json.RawMessage, others []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for _, o := range others {
		if !bytes.Equal(o, val) && !containsRaw(out, o) {
			out = append(out, o)
			if len(out) == 3 {
				break
			}
		}
	}
	return out
}

func fieldReplacementsWith(val json.RawMessage, others []json.RawMessage, skipStringNearby bool, opts mutationOpts) []json.RawMessage {
	out := seenReplacementsOnly(val, others)
	for _, v := range nearbyWith(val, skipStringNearby, opts) {
		if !bytes.Equal(v, val) && !containsRaw(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// elemReplacementsWith returns replacement values for elems[ei]: up to 3 values at
// the same index in other inputs (same), up to 3 values at other indices of
// earlier arrays that contain elems[ei] at another index (cross), and nearby
// values (near).
func elemReplacementsWith(elems []json.RawMessage, ei int, others, recent []json.RawMessage, skipStringNearby bool, opts mutationOpts) (same, cross, near []json.RawMessage) {
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
	for _, v := range nearbyWith(e, skipStringNearby, opts) {
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
		s[k].input.Data = m.data
		out = append(out, candidate{fmt.Sprintf("failing input (seq %d): %s", base[k].input.Seq, m.desc), s})
	}
	return out
}

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

var isoCurrencies = map[string]bool{
	"USD": true, "EUR": true, "GBP": true, "JPY": true, "BHD": true,
	"CLF": true, "CHF": true, "CAD": true, "AUD": true, "INR": true,
	"SGD": true, "CNY": true, "HKD": true, "KWD": true, "OMR": true,
	"JOD": true, "TND": true, "NZD": true, "SEK": true, "NOK": true,
	"DKK": true, "KRW": true, "BRL": true, "MXN": true, "ZAR": true,
	"PLN": true, "AED": true, "SAR": true, "THB": true, "IDR": true,
	"MYR": true, "PHP": true, "VND": true, "TWD": true, "ILS": true,
	"CZK": true, "HUF": true, "CLP": true, "COP": true, "ARS": true,
	"EGP": true, "NGN": true, "KES": true, "PKR": true, "BDT": true,
	"LKR": true, "QAR": true, "ISK": true, "RON": true, "BGN": true,
	"HRK": true, "RSD": true, "UAH": true,
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

func parseRFC3339(s string) (t time.Time, hasSubSec bool, ok bool) {
	if len(s) < 20 || !strings.ContainsAny(s, "Tt") {
		return time.Time{}, false, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false, false
	}
	tIdx := strings.IndexAny(s, "Tt")
	return parsed, strings.ContainsRune(s[tIdx:], '.'), true
}

var timestampShifts = []time.Duration{
	time.Millisecond,
	-time.Millisecond,
	time.Second,
	-time.Second,
}

func nearbyWith(v json.RawMessage, skipStringNearby bool, opts mutationOpts) []json.RawMessage {
	switch {
	case len(v) == 0:
		return nil
	case v[0] == '"':
		var s string
		if json.Unmarshal(v, &s) != nil {
			return nil
		}
		if ts, hasSubSec, ok := parseRFC3339(s); ok {
			var out []json.RawMessage
			seen := map[string]bool{s: true}
			for _, d := range timestampShifts {
				shifted := ts.Add(d)
				layout := time.RFC3339
				if hasSubSec || shifted.Nanosecond() != 0 || d == time.Millisecond || d == -time.Millisecond {
					layout = time.RFC3339Nano
				}
				formatted := shifted.Format(layout)
				if !seen[formatted] {
					seen[formatted] = true
					out = append(out, marshalJSONNoEscape(formatted))
				}
			}
			return out
		}
		if skipStringNearby || isUUID(s) || isoCurrencies[s] {
			return nil
		}
		return []json.RawMessage{marshalJSONNoEscape(s + "-1")}
	case string(v) == "true":
		return []json.RawMessage{json.RawMessage("false")}
	case string(v) == "false":
		return []json.RawMessage{json.RawMessage("true")}
	}
	if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
		var cand []int64
		if opts.signPreserving && n > 0 {
			if n < math.MaxInt64 {
				cand = append(cand, n+1)
			}
			if n > 1 {
				cand = append(cand, n-1)
			}
			cand = append(cand, 1)
			if n <= math.MaxInt64/2 {
				cand = append(cand, n*2)
			}
		} else {
			if n < math.MaxInt64 {
				cand = append(cand, n+1)
			}
			if n != 0 && n != math.MinInt64 {
				cand = append(cand, -n)
			}
			if n > math.MinInt64 {
				cand = append(cand, n-1)
			}
			cand = append(cand, 0, 1)
			if n <= math.MaxInt64/2 && n >= math.MinInt64/2 {
				cand = append(cand, n*2)
			}
		}
		seen := map[int64]bool{n: true}
		var out []json.RawMessage
		for _, c := range cand {
			if !seen[c] {
				seen[c] = true
				out = append(out, json.RawMessage(strconv.FormatInt(c, 10)))
			}
		}
		return out
	}
	if f, err := strconv.ParseFloat(string(v), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		type fc struct {
			val   float64
			delta bool
		}
		var cand []fc
		if opts.signPreserving && f > 0 {
			cand = []fc{{f * 2, false}, {f + 0.01, true}}
			if f-0.01 > 0 {
				cand = append(cand, fc{f - 0.01, true})
			}
		} else {
			cand = []fc{
				{f * 2, false},
				{f + 0.01, true},
				{f - 0.01, true},
				{0, false},
			}
		}
		seenF := map[float64]bool{f: true}
		seenS := map[string]bool{string(v): true}
		var out []json.RawMessage
		for _, c := range cand {
			if math.IsNaN(c.val) || math.IsInf(c.val, 0) || seenF[c.val] {
				continue
			}
			var s string
			if c.delta && math.Abs(c.val) < 1e9 {
				s = strconv.FormatFloat(c.val, 'g', 12, 64)
			} else {
				s = strconv.FormatFloat(c.val, 'g', -1, 64)
			}
			if !seenS[s] {
				seenF[c.val] = true
				seenS[s] = true
				out = append(out, json.RawMessage(s))
			}
		}
		return out
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

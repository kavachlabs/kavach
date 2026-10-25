package harbor

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
)

// The contract a trial's collect hook copied out of its container, and the
// limits on what a variant's decoding returns.
const (
	contractSubdir   = "artifacts/logs/artifacts/contract"
	maxVariantText   = 4000 // bytes of one text field
	maxVariantBinary = 3000 // bytes of a field shown as base64 (4000 characters)
	maxVariantSteps  = 2000
)

// VariantList is what a trial's contract checks saved: one line per check
// from rounds.jsonl, and every failing variant saved under rounds/<round>/.
type VariantList struct {
	Rounds   []ContractRound `json:"rounds"`
	Variants []VariantInfo   `json:"variants"` // by round, then id
	Note     string          `json:"note,omitempty"`
}

// ContractRound is one line of the contract's rounds.jsonl.
type ContractRound struct {
	Round   int    `json:"round"`
	At      string `json:"at"`
	Build   string `json:"build,omitempty"`
	Verdict string `json:"verdict"`
	Changed int    `json:"changed"`
}

// VariantInfo is one saved variant journal.
type VariantInfo struct {
	Round int `json:"round"`
	ID    int `json:"id"`
	// HasResult is set when the new build's replay of it (variant-NN.json)
	// was saved; Verdict is that replay's verdict.
	HasResult bool             `json:"has_result"`
	Verdict   string           `json:"verdict,omitempty"`
	Service   string           `json:"service,omitempty"`
	Variant   *journal.Variant `json:"variant"` // null when the header has none
	Error     string           `json:"error,omitempty"`
}

// VariantReplay is one saved variant decoded into steps, with the new build's
// replay of it.
type VariantReplay struct {
	Round   int              `json:"round"`
	ID      int              `json:"id"`
	Service string           `json:"service"`
	Start   string           `json:"start"`
	Variant *journal.Variant `json:"variant"`
	Records int              `json:"records"`
	Steps   []VariantStep    `json:"steps"`
	// Truncated is set when the variant has more than 2000 steps; Steps and
	// New.Steps hold the first 2000.
	Truncated bool `json:"truncated,omitempty"`
	// JournalTruncated is set when the file ends inside a block; Steps hold
	// what its complete blocks recorded.
	JournalTruncated bool           `json:"journal_truncated,omitempty"`
	New              *VariantResult `json:"new"`
	NewError         string         `json:"new_error,omitempty"` // the .json exists but does not decode
}

// VariantStep is one input of a variant and everything recorded for it.
type VariantStep struct {
	Seq         uint64           `json:"seq"`
	Source      string           `json:"source"`
	Position    string           `json:"position"`
	Input       string           `json:"input"`
	InputBase64 bool             `json:"input_base64"`
	Clock       []int64          `json:"clock"`  // UnixNanos of each clock read, in order
	Random      int              `json:"random"` // number of random reads
	Gateways    []VariantGateway `json:"gateways"`
	Config      []VariantConfig  `json:"config"`
	// Recorded are the outputs the variant journal holds for the step. A
	// variant never happened, so Kavach writes none; Traffic are the outputs
	// the contract's recorded traffic holds for the same input (by source and
	// position), when it was collected.
	Recorded   []VariantOutput `json:"recorded"`
	Traffic    []VariantOutput `json:"traffic,omitempty"`
	TrafficSeq *uint64         `json:"traffic_seq,omitempty"`
	// InputChanged is set when the input's data differs from the traffic's.
	InputChanged bool           `json:"input_changed,omitempty"`
	Marker       *VariantMarker `json:"marker,omitempty"`
}

// VariantOutput is one output: its sink and data.
type VariantOutput struct {
	Sink       string `json:"sink"`
	Data       string `json:"data"`
	DataBase64 bool   `json:"data_base64,omitempty"`
	Local      bool   `json:"local,omitempty"` // scope local (SPEC.md §4.4)
}

// VariantGateway is one gateway query recorded in a step.
type VariantGateway struct {
	Gateway        string `json:"gateway"`
	Request        string `json:"request"`
	RequestBase64  bool   `json:"request_base64,omitempty"`
	Response       string `json:"response"`
	ResponseBase64 bool   `json:"response_base64,omitempty"`
	Error          string `json:"error"`
	Local          bool   `json:"local,omitempty"`
}

// VariantConfig is one config read recorded in a step.
type VariantConfig struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	ValueBase64 bool   `json:"value_base64,omitempty"`
	Present     bool   `json:"present"`
	Source      string `json:"source,omitempty"`
}

// VariantMarker is how the recording failed, on the step it failed at.
type VariantMarker struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// VariantResult is the new build's replay of a variant (replay.Result).
type VariantResult struct {
	Verdict   string              `json:"verdict"`
	Status    string              `json:"status"`
	Seq       *uint64             `json:"seq,omitempty"`
	Invariant string              `json:"invariant,omitempty"`
	Detail    string              `json:"detail,omitempty"`
	Steps     []VariantResultStep `json:"steps"`
	// Changed and Unreached are set on a replay of a contract's traffic: the
	// feature steps whose outputs changed, and those that kept the recorded
	// outputs.
	Changed   []uint64 `json:"changed,omitempty"`
	Unreached []uint64 `json:"unreached,omitempty"`
	// Invariants are the invariants the build declared.
	Invariants []string `json:"invariants,omitempty"`
}

// VariantResultStep is what one step produced in the new build's replay.
type VariantResultStep struct {
	Seq         uint64          `json:"seq"`
	Outputs     []VariantOutput `json:"outputs"`
	Panic       string          `json:"panic"`
	Error       string          `json:"error"`
	Crash       string          `json:"crash,omitempty"`
	Timeout     string          `json:"timeout,omitempty"`
	Synthesized int             `json:"synthesized_reads,omitempty"`
}

var variantFile = regexp.MustCompile(`^variant-(\d{1,9})\.kavach$`)

// TrialVariants lists the variants a trial's contract checks saved, and the
// rounds of those checks. A trial without a collected contract has none.
func (r *Reader) TrialVariants(job, id string) (*VariantList, error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return nil, err
	}
	cdir := filepath.Join(dir, filepath.FromSlash(contractSubdir))
	list := &VariantList{Rounds: []ContractRound{}, Variants: []VariantInfo{}}
	if st, err := os.Stat(cdir); err != nil || !st.IsDir() {
		list.Note = "no Kavach contract was collected from this trial"
		return list, nil
	}
	list.Rounds = readRounds(filepath.Join(cdir, "rounds.jsonl"))

	rounds, _ := os.ReadDir(filepath.Join(cdir, "rounds"))
	for _, rd := range rounds {
		n, ok := canonicalInt(rd.Name())
		if !ok || !rd.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(cdir, "rounds", rd.Name()))
		for _, f := range files {
			m := variantFile.FindStringSubmatch(f.Name())
			if m == nil || f.IsDir() {
				continue
			}
			vid, _ := strconv.Atoi(m[1])
			path := filepath.Join(cdir, "rounds", rd.Name(), f.Name())
			info := VariantInfo{Round: n, ID: vid}
			if res, err := readResult(resultPath(path)); err == nil {
				info.HasResult, info.Verdict = true, res.String()
			} else if !errors.Is(err, os.ErrNotExist) {
				info.HasResult, info.Error = true, err.Error()
			}
			if hdr, err := readHeader(path); err != nil {
				info.Error = err.Error() // a journal error outranks a result one
			} else {
				info.Service, info.Variant = hdr.Meta.Service, hdr.Meta.Variant
			}
			list.Variants = append(list.Variants, info)
		}
	}
	sort.Slice(list.Variants, func(i, j int) bool {
		a, b := list.Variants[i], list.Variants[j]
		if a.Round != b.Round {
			return a.Round < b.Round
		}
		return a.ID < b.ID
	})
	if len(list.Variants) == 0 {
		list.Note = "the contract checks saved no failing variants"
	}
	return list, nil
}

// TrialVariant decodes variant vid of round into steps, with the new build's
// replay of it when that was saved.
func (r *Reader) TrialVariant(job, id string, round, vid int) (*VariantReplay, error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return nil, err
	}
	cdir := filepath.Join(dir, filepath.FromSlash(contractSubdir))
	path := filepath.Join(cdir, "rounds", strconv.Itoa(round), fmt.Sprintf("variant-%02d.kavach", vid))
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	j, err := journal.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("variant %d of round %d: %w", vid, round, err)
	}
	m := j.Header.Meta
	out := &VariantReplay{
		Round: round, ID: vid, Service: m.Service, Start: m.Start, Variant: m.Variant,
		Records: len(j.Records), JournalTruncated: j.Truncated,
	}
	out.Steps, out.Truncated = variantSteps(j.Records)
	if t := readTraffic(filepath.Join(cdir, "traffic.kavach")); t != nil {
		for i := range out.Steps {
			s := &out.Steps[i]
			ts, ok := t[s.Source+"\x00"+s.Position]
			if !ok {
				continue
			}
			seq := ts.seq
			s.TrafficSeq, s.Traffic, s.InputChanged = &seq, ts.outputs, ts.input != s.Input
		}
	}
	res, err := readResult(resultPath(path))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		out.NewError = err.Error()
	default:
		var cut bool
		out.New, cut = variantResult(res)
		out.Truncated = out.Truncated || cut
	}
	return out, nil
}

// variantSteps groups records into steps: an input starts one, and the reads,
// outputs and marker after it belong to it (SPEC.md §6).
func variantSteps(recs []journal.Record) ([]VariantStep, bool) {
	steps := []VariantStep{}
	var cur *VariantStep
	truncated := false
	for _, rec := range recs {
		if rec.Type == journal.TypeInput {
			if len(steps) == maxVariantSteps {
				truncated = true
				break
			}
			in, b64 := text(rec.Data)
			steps = append(steps, VariantStep{
				Seq: rec.Seq, Source: rec.Source, Position: rec.Position, Input: in, InputBase64: b64,
				Clock: []int64{}, Gateways: []VariantGateway{}, Config: []VariantConfig{}, Recorded: []VariantOutput{},
			})
			cur = &steps[len(steps)-1]
			continue
		}
		if cur == nil {
			continue // snapshot and genesis environment
		}
		switch rec.Type {
		case journal.TypeClock:
			cur.Clock = append(cur.Clock, rec.UnixNanos)
		case journal.TypeRand:
			cur.Random++
		case journal.TypeGateway:
			g := VariantGateway{Gateway: rec.Gateway, Error: clipTo(rec.Error, maxVariantText), Local: rec.Scope == journal.ScopeLocal}
			g.Request, g.RequestBase64 = text(rec.Request)
			g.Response, g.ResponseBase64 = text(rec.Response)
			cur.Gateways = append(cur.Gateways, g)
		case journal.TypeConfig:
			c := VariantConfig{Key: clipTo(rec.Key, maxVariantText), Present: rec.Present, Source: rec.Source}
			c.Value, c.ValueBase64 = text(rec.Value)
			cur.Config = append(cur.Config, c)
		case journal.TypeOutput:
			cur.Recorded = append(cur.Recorded, output(rec.Sink, rec.Data, rec.Scope == journal.ScopeLocal))
		case journal.TypeMarker:
			if cur.Marker == nil && isFailureMarker(rec.Kind) {
				cur.Marker = &VariantMarker{Kind: rec.Kind, Message: clipTo(rec.Message, maxVariantText)}
			}
		}
	}
	return steps, truncated
}

func isFailureMarker(kind string) bool {
	switch kind {
	case journal.MarkerPanic, journal.MarkerError, journal.MarkerInvariant, journal.MarkerCrash:
		return true
	}
	return false
}

func variantResult(res *replay.Result) (*VariantResult, bool) {
	v := &VariantResult{
		Verdict: res.String(), Status: string(res.Status), Seq: res.Seq, Invariant: res.Invariant,
		Detail: clipTo(res.Detail, maxVariantText), Steps: []VariantResultStep{}, Unreached: res.Unreached, Invariants: res.Invariants,
	}
	for _, c := range res.Changed {
		v.Changed = append(v.Changed, c.Seq)
	}
	cut := false
	for _, s := range res.Steps {
		if len(v.Steps) == maxVariantSteps {
			cut = true
			break
		}
		rs := VariantResultStep{
			Seq: s.Seq, Outputs: []VariantOutput{}, Panic: clipTo(s.Panic, maxVariantText), Error: clipTo(s.Error, maxVariantText),
			Crash: clipTo(s.Crash, maxVariantText), Timeout: clipTo(s.Timeout, maxVariantText), Synthesized: s.Synthesized,
		}
		for _, o := range s.Outputs {
			rs.Outputs = append(rs.Outputs, output(o.Sink, o.Data, false))
		}
		v.Steps = append(v.Steps, rs)
	}
	return v, cut
}

func output(sink string, data []byte, local bool) VariantOutput {
	o := VariantOutput{Sink: clipTo(sink, maxVariantText), Local: local}
	o.Data, o.DataBase64 = text(data)
	return o
}

// text is b as UTF-8 text cut to 4000 bytes, or, when it is not valid UTF-8,
// its first 3000 bytes in base64.
func text(b []byte) (string, bool) {
	if utf8.Valid(b) {
		return clipTo(string(b), maxVariantText), false
	}
	if len(b) > maxVariantBinary {
		b = b[:maxVariantBinary]
	}
	return base64.StdEncoding.EncodeToString(b), true
}

// trafficStep is what the contract's traffic recorded for one input.
type trafficStep struct {
	seq     uint64
	input   string
	outputs []VariantOutput
}

// readTraffic indexes the steps of a contract's traffic by source and
// position. Inputs whose source and position repeat are left out. It returns
// nil when the traffic is missing or unreadable.
func readTraffic(path string) map[string]trafficStep {
	j, err := journal.ReadFile(path)
	if err != nil {
		return nil
	}
	steps, _ := variantSteps(j.Records)
	idx := make(map[string]trafficStep, len(steps))
	dup := map[string]bool{}
	for _, s := range steps {
		k := s.Source + "\x00" + s.Position
		if _, ok := idx[k]; ok || dup[k] {
			delete(idx, k)
			dup[k] = true
			continue
		}
		idx[k] = trafficStep{seq: s.Seq, input: s.Input, outputs: s.Recorded}
	}
	return idx
}

func readRounds(path string) []ContractRound {
	rounds := []ContractRound{}
	f, err := os.Open(path)
	if err != nil {
		return rounds
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r ContractRound
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Round > 0 {
			r.Build = clipTo(r.Build, 600)
			rounds = append(rounds, r)
		}
	}
	return rounds
}

// readHeader reads only a journal's header.
func readHeader(path string) (journal.Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return journal.Header{}, err
	}
	defer f.Close()
	jr, err := journal.NewReader(bufio.NewReader(f))
	if err != nil {
		return journal.Header{}, err
	}
	return jr.Header(), nil
}

func resultPath(variant string) string {
	return variant[:len(variant)-len(".kavach")] + ".json"
}

func readResult(path string) (*replay.Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var res replay.Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return &res, nil
}

// canonicalInt parses a non-negative decimal written without leading zeros.
func canonicalInt(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 1e9 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}

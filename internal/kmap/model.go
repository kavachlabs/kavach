package kmap

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
)

// Input configures building a Model.
type Input struct {
	Spec     *Spec
	Fixtures []string
	Bin      string
	New      string
	Diffs    []string
	Overlays []string
	MaxData  int
	BaseDir  string
	Version  string
	Title    string
	Replay   func(j *journal.Journal, bin string) (*replay.Result, error)
}

// Model is the unified data/call/invariant/overlay JSON structure embedded in
// the generated HTML or printed by `kavach map --json`.
type Model struct {
	Version   int                   `json:"kavach_map_model"`
	Generator string                `json:"generator"`
	Title     string                `json:"title"`
	Summary   string                `json:"summary,omitempty"`
	Groups    []Group               `json:"groups"`
	Nodes     []Node                `json:"nodes"`
	Edges     []Edge                `json:"edges"`
	Flows     []Flow                `json:"flows,omitempty"`
	Fixtures  []Fixture             `json:"fixtures,omitempty"`
	Overlays  []Overlay             `json:"overlays,omitempty"`
	Stats     map[string]*NodeStats `json:"stats"`
	Warnings  []string              `json:"warnings,omitempty"`
}

// Fixture is one recorded .kavach incident or baseline journal plus its runs and diff.
type Fixture struct {
	ID       string       `json:"id"`
	Label    string       `json:"label"`
	Path     string       `json:"path"`
	Service  string       `json:"service,omitempty"`
	Origin   string       `json:"origin"` // "arg" | "overlay"
	Recorded *Failure     `json:"recorded,omitempty"`
	Runs     []Run        `json:"runs"`
	Diff     *DiffSummary `json:"diff,omitempty"`
}

// Failure summarizes the failure marker recorded in a fixture.
type Failure struct {
	Seq     uint64 `json:"seq"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Run is one execution trace (recorded or replayed) over a fixture.
type Run struct {
	ID         string             `json:"id"`
	Mode       string             `json:"mode"`            // "recorded" | "replay"
	Build      string             `json:"build,omitempty"` // "" | "old" | "new"
	Bin        string             `json:"bin,omitempty"`
	Verdict    string             `json:"verdict"`
	Status     string             `json:"status"`
	Detail     string             `json:"detail,omitempty"`
	Seq        *uint64            `json:"seq,omitempty"`
	Invariants []string           `json:"invariants,omitempty"`
	Declared   []string           `json:"declared,omitempty"`
	Steps      []Step             `json:"steps"`
	Divergence *replay.Divergence `json:"divergence,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// Step is one input step in a run.
type Step struct {
	Seq            uint64            `json:"seq"`
	Source         string            `json:"source"`
	Position       string            `json:"position"`
	Input          string            `json:"input"`
	InputTruncated bool              `json:"input_truncated,omitempty"`
	Omitted        bool              `json:"omitted,omitempty"`
	Outputs        []Out             `json:"outputs,omitempty"`
	Reads          map[string]int    `json:"reads,omitempty"`
	Outcome        string            `json:"outcome"`
	Message        string            `json:"message,omitempty"`
	Hits           []Hit             `json:"hits,omitempty"`
	Invariants     map[string]string `json:"invariants,omitempty"`
	Detected       []string          `json:"detected,omitempty"`
}

// Out is one emitted output in a step.
type Out struct {
	Sink      string `json:"sink"`
	Data      string `json:"data"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Hit links a step (or specific output) to a map node.
type Hit struct {
	Node   string `json:"node"`
	Role   string `json:"role"`   // "input" | "output" | "failure"
	Output int    `json:"output"` // >=0 for output, -1 otherwise
}

// NodeStats aggregates hits and invariant violations per node.
type NodeStats struct {
	Hits        []StatRef `json:"hits,omitempty"`
	Violations  []StatRef `json:"violations,omitempty"`
	VariantsOld int       `json:"variants_old,omitempty"`
	VariantsNew int       `json:"variants_new,omitempty"`
}

// StatRef identifies a step inside a fixture run.
type StatRef struct {
	Fixture string `json:"fixture"`
	Run     string `json:"run"`
	Seq     uint64 `json:"seq"`
}

type rawOut struct {
	Sink string
	Data []byte
}

type rawStep struct {
	Seq      uint64
	Source   string
	Position string
	Data     []byte
	Reads    map[string]int
	Outputs  []rawOut
	Outcome  string
	Message  string
}

type fixtureData struct {
	fix      Fixture
	absPath  string
	journal  *journal.Journal
	recSteps []rawStep
	runs     []rawRunData
}

type rawRunData struct {
	run          Run
	stepRawOuts  [][]rawOut
	stepRawInput [][]byte
	failInvName  string
}

// Build constructs a Model from an optional Spec, fixtures, optional replay binaries,
// diff JSON reports, and stage overlays.
func Build(in Input) (*Model, error) {
	baseDir := in.BaseDir
	if baseDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		baseDir = wd
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, err
	}

	replayFn := in.Replay
	if replayFn == nil {
		replayFn = func(j *journal.Journal, bin string) (*replay.Result, error) {
			return replay.RunHostWith(j, bin, replay.Options{})
		}
	}

	var spec *Spec
	if in.Spec != nil {
		cp := cloneSpec(in.Spec)
		if err := cp.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		spec = cp
	}

	m := &Model{
		Version:   1,
		Generator: "kavach",
		Stats:     map[string]*NodeStats{},
	}
	if in.Version != "" {
		m.Generator = "kavach " + in.Version
	}

	// 1. Load overlays first so any stage fixtures are discovered and deduplicated.
	type loadedOverlay struct {
		ov     *Overlay
		absDir string
	}
	var loadedOverlays []loadedOverlay
	type fixtureReq struct {
		absPath string
		relPath string
		origin  string
	}
	var fixReqs []fixtureReq
	fixIdxByAbs := map[string]int{}

	addFixtureReq := func(rawPath, origin string) (int, error) {
		abs, err := filepath.Abs(rawPath)
		if err != nil {
			return -1, err
		}
		if idx, exists := fixIdxByAbs[abs]; exists {
			return idx, nil
		}
		idx := len(fixReqs)
		fixIdxByAbs[abs] = idx
		fixReqs = append(fixReqs, fixtureReq{
			absPath: abs,
			relPath: relPath(absBase, abs),
			origin:  origin,
		})
		return idx, nil
	}

	for _, f := range in.Fixtures {
		if _, err := addFixtureReq(f, "arg"); err != nil {
			return nil, err
		}
	}

	// Map (overlayIndex, laneIndex, stageIndex) -> fixtureReq index (-1 if none).
	stageFixIdx := map[[3]int]int{}
	for oi, ovPath := range in.Overlays {
		absOv, err := filepath.Abs(ovPath)
		if err != nil {
			return nil, err
		}
		ov, err := LoadOverlay(absOv)
		if err != nil {
			return nil, err
		}
		ov.Source = relPath(absBase, absOv)
		ovDir := filepath.Dir(absOv)
		for li := range ov.Lanes {
			for si := range ov.Lanes[li].Stages {
				st := &ov.Lanes[li].Stages[si]
				if st.Fixture == "" {
					continue
				}
				cand := st.Fixture
				if !filepath.IsAbs(cand) {
					cand = filepath.Join(ovDir, cand)
				}
				fidx, err := addFixtureReq(cand, "overlay")
				if err != nil {
					return nil, err
				}
				st.Fixture = fixReqs[fidx].relPath
				stageFixIdx[[3]int{oi, li, si}] = fidx
			}
		}
		loadedOverlays = append(loadedOverlays, loadedOverlay{ov: ov, absDir: ovDir})
	}

	// Assign unique IDs and Labels to fixtures.
	baseCounts := map[string]int{}
	for _, fr := range fixReqs {
		baseCounts[filepath.Base(fr.absPath)]++
	}
	usedIDs := map[string]int{}
	fixIDs := make([]string, len(fixReqs))
	fixLabels := make([]string, len(fixReqs))
	for i, fr := range fixReqs {
		base := filepath.Base(fr.absPath)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		if stem == "" {
			stem = "fixture"
		}
		usedIDs[stem]++
		id := stem
		if usedIDs[stem] > 1 {
			id = fmt.Sprintf("%s-%d", stem, usedIDs[stem])
		}
		fixIDs[i] = id
		if baseCounts[base] > 1 {
			fixLabels[i] = fr.relPath
		} else {
			fixLabels[i] = base
		}
	}

	// 2. Read fixtures, extract recorded steps, and run optional replays.
	fixturesData := make([]*fixtureData, len(fixReqs))
	for i, fr := range fixReqs {
		j, err := journal.ReadFile(fr.absPath)
		if err != nil {
			return nil, err
		}
		recSteps, recFailure := extractRecordedSteps(j.Records)
		fd := &fixtureData{
			fix: Fixture{
				ID:       fixIDs[i],
				Label:    fixLabels[i],
				Path:     fr.relPath,
				Service:  j.Header.Meta.Service,
				Origin:   fr.origin,
				Recorded: recFailure,
			},
			absPath:  fr.absPath,
			journal:  j,
			recSteps: recSteps,
		}
		fd.runs = append(fd.runs, buildRecordedRun(recSteps, recFailure, in.MaxData))

		var oldRes, newRes *replay.Result
		if in.Bin != "" {
			r, rerr := replayFn(j, in.Bin)
			if rerr == nil {
				oldRes = r
			}
			fd.runs = append(fd.runs, buildReplayRun("old", "old", in.Bin, recSteps, r, rerr, in.MaxData, nil))
		}
		if in.New != "" {
			r, rerr := replayFn(j, in.New)
			if rerr == nil {
				newRes = r
			}
			var div *replay.Divergence
			if oldRes != nil && newRes != nil {
				div = replay.Compare(oldRes, newRes)
			}
			fd.runs = append(fd.runs, buildReplayRun("new", "new", in.New, recSteps, r, rerr, in.MaxData, div))
		}
		fixturesData[i] = fd
	}

	// 3. Load and attach diff reports by fixture basename.
	for _, dp := range in.Diffs {
		ds, rawFix, err := LoadDiff(dp)
		if err != nil {
			return nil, err
		}
		ds.Source = relPath(absBase, dp)
		wantBase := filepath.Base(rawFix)
		matched := 0
		for _, fd := range fixturesData {
			if filepath.Base(fd.absPath) == wantBase {
				cp := *ds
				fd.fix.Diff = &cp
				matched++
			}
		}
		if matched == 0 {
			m.Warnings = append(m.Warnings, fmt.Sprintf("diff %s (%s) matched no fixture", ds.Source, wantBase))
		}
	}

	// 4. Populate spec nodes/edges/flows and synthesize auto/unmapped nodes & edges.
	if spec != nil {
		m.Groups = append(m.Groups, spec.Groups...)
		m.Nodes = append(m.Nodes, spec.Nodes...)
		m.Edges = append(m.Edges, spec.Edges...)
		m.Flows = append(m.Flows, spec.Flows...)
		m.Title = spec.Title
		m.Summary = spec.Summary
	}

	// Evaluate hits on spec nodes first so we know what runtime entities are unmapped.
	if spec != nil {
		evalNodeRules(m.Nodes, fixturesData)
	}

	synthesizeUnmapped(m, spec == nil, fixturesData)

	// Evaluate rules across all nodes (including newly synthesized nodes).
	evalNodeRules(m.Nodes, fixturesData)

	// Build auto edges involving synthesized nodes.
	addAutoEdges(m, spec == nil, fixturesData)

	// Compute invariant matrices, detection, and failure hits on invariant nodes.
	computeInvariantsAndDetection(m.Nodes, fixturesData)

	// Assemble final fixtures, stats, and overlays.
	for _, n := range m.Nodes {
		m.Stats[n.ID] = &NodeStats{}
	}

	m.Fixtures = make([]Fixture, len(fixturesData))
	for i, fd := range fixturesData {
		fix := fd.fix
		fix.Runs = make([]Run, len(fd.runs))
		for ri, rd := range fd.runs {
			fix.Runs[ri] = rd.run
		}
		m.Fixtures[i] = fix
	}

	computeNodeStats(m)

	// Link overlay stage Runs, validate stage Nodes, resolve Item.Sink, and merge by mode.
	nodeSet := make(map[string]bool, len(m.Nodes))
	for _, n := range m.Nodes {
		nodeSet[n.ID] = true
	}
	fixByID := make(map[string]*Fixture, len(m.Fixtures))
	for i := range m.Fixtures {
		fixByID[m.Fixtures[i].ID] = &m.Fixtures[i]
	}

	mergedOverlays := map[string]int{}
	for oi, lo := range loadedOverlays {
		ov := lo.ov
		for li := range ov.Lanes {
			for si := range ov.Lanes[li].Stages {
				st := &ov.Lanes[li].Stages[si]
				if fidx, ok := stageFixIdx[[3]int{oi, li, si}]; ok {
					st.Runs = []string{fixIDs[fidx]}
				}
				if len(st.Nodes) > 0 {
					kept := make([]string, 0, len(st.Nodes))
					for _, nid := range st.Nodes {
						if nodeSet[nid] {
							kept = append(kept, nid)
						} else {
							m.Warnings = append(m.Warnings, fmt.Sprintf("overlay %s stage %q: unknown node %q", ov.Source, st.ID, nid))
						}
					}
					st.Nodes = kept
				} else if len(st.Runs) > 0 {
					if f := fixByID[st.Runs[0]]; f != nil {
						st.Nodes = fallbackStageNodes(f)
					}
				}
				for ii := range st.Items {
					it := &st.Items[ii]
					if it.Sink != "" {
						it.Nodes = resolveSinkNodes(m.Nodes, it.Sink, it.Text)
					}
				}
			}
		}

		if idx, exists := mergedOverlays[ov.Mode]; exists {
			dst := &m.Overlays[idx]
			dst.Lanes = append(dst.Lanes, ov.Lanes...)
			if dst.Summary == "" {
				dst.Summary = ov.Summary
			}
			if ov.Source != "" && !strings.Contains(dst.Source, ov.Source) {
				dst.Source += ", " + ov.Source
			}
		} else {
			mergedOverlays[ov.Mode] = len(m.Overlays)
			m.Overlays = append(m.Overlays, *ov)
		}
	}

	// Title and summary fallbacks.
	if in.Title != "" {
		m.Title = in.Title
	}
	if m.Title == "" {
		svc := ""
		for _, f := range m.Fixtures {
			if f.Service != "" {
				svc = f.Service
				break
			}
		}
		if svc != "" {
			m.Title = svc + " map"
		} else if len(m.Overlays) > 0 && m.Overlays[0].Title != "" {
			m.Title = m.Overlays[0].Title
		} else {
			m.Title = "Kavach Map"
		}
	}
	if m.Summary == "" && len(m.Fixtures) > 0 {
		m.Summary = fmt.Sprintf("%d fixture(s) · %d node(s) · %d edge(s)", len(m.Fixtures), len(m.Nodes), len(m.Edges))
	}

	return m, nil
}

func cloneSpec(s *Spec) *Spec {
	cp := *s
	cp.Groups = append([]Group(nil), s.Groups...)
	cp.Nodes = make([]Node, len(s.Nodes))
	for i, n := range s.Nodes {
		n.Code = append([]string(nil), n.Code...)
		n.Links = append([]CodeLink(nil), n.Links...)
		n.Match = append([]Rule(nil), n.Match...)
		for j := range n.Match {
			if len(n.Match[j].JSON) > 0 {
				jm := make(map[string]any, len(n.Match[j].JSON))
				for k, v := range n.Match[j].JSON {
					jm[k] = v
				}
				n.Match[j].JSON = jm
			}
		}
		cp.Nodes[i] = n
	}
	cp.Edges = append([]Edge(nil), s.Edges...)
	cp.Flows = make([]Flow, len(s.Flows))
	for i, f := range s.Flows {
		f.Steps = append([]FlowStep(nil), f.Steps...)
		cp.Flows[i] = f
	}
	return &cp
}

func relPath(absBase, target string) string {
	abs, err := filepath.Abs(target)
	if err == nil {
		if rel, err := filepath.Rel(absBase, abs); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(target)
}

func extractRecordedSteps(recs []journal.Record) ([]rawStep, *Failure) {
	var steps []rawStep
	var cur *rawStep
	var fail *Failure

	for _, r := range recs {
		switch r.Type {
		case journal.TypeEnvironment, journal.TypeSnapshot:
			continue
		case journal.TypeInput:
			if cur != nil {
				if cur.Outcome == "" {
					cur.Outcome = "ok"
				}
				steps = append(steps, *cur)
			}
			cur = &rawStep{
				Seq:      r.Seq,
				Source:   r.Source,
				Position: r.Position,
				Data:     r.Data,
				Reads:    map[string]int{},
			}
		case journal.TypeClock:
			if cur != nil {
				cur.Reads["clock"]++
			}
		case journal.TypeRand:
			if cur != nil {
				cur.Reads["rand"]++
			}
		case journal.TypeGateway:
			if cur != nil {
				cur.Reads["gateway"]++
			}
		case journal.TypeConfig:
			if cur != nil {
				cur.Reads["config"]++
			}
		case journal.TypeOutput:
			if cur != nil {
				cur.Outputs = append(cur.Outputs, rawOut{Sink: r.Sink, Data: r.Data})
			}
		case journal.TypeMarker:
			if cur == nil {
				continue
			}
			switch r.Kind {
			case journal.MarkerPanic, journal.MarkerError, journal.MarkerInvariant, journal.MarkerCrash:
				cur.Outcome = r.Kind
				cur.Message = r.Message
				fail = &Failure{Seq: cur.Seq, Kind: r.Kind, Message: r.Message}
			}
		}
	}
	if cur != nil {
		if cur.Outcome == "" {
			cur.Outcome = "ok"
		}
		steps = append(steps, *cur)
	}
	return steps, fail
}

func buildRecordedRun(recSteps []rawStep, recFail *Failure, maxData int) rawRunData {
	steps := make([]Step, len(recSteps))
	rawOuts := make([][]rawOut, len(recSteps))
	rawIns := make([][]byte, len(recSteps))

	for i, rs := range recSteps {
		inStr, inTrunc, omitted := previewData(rs.Data, maxData)
		outs := make([]Out, len(rs.Outputs))
		for oi, ro := range rs.Outputs {
			d, tr, _ := previewData(ro.Data, maxData)
			outs[oi] = Out{Sink: ro.Sink, Data: d, Truncated: tr}
		}
		steps[i] = Step{
			Seq:            rs.Seq,
			Source:         rs.Source,
			Position:       rs.Position,
			Input:          inStr,
			InputTruncated: inTrunc,
			Omitted:        omitted,
			Outputs:        outs,
			Reads:          nonEmptyReads(rs.Reads),
			Outcome:        rs.Outcome,
			Message:        rs.Message,
		}
		rawOuts[i] = rs.Outputs
		rawIns[i] = rs.Data
	}

	r := Run{
		ID:      "recorded",
		Mode:    "recorded",
		Verdict: "ok",
		Status:  "ok",
		Steps:   steps,
	}
	var failInv string
	if recFail != nil {
		seq := recFail.Seq
		r.Verdict = fmt.Sprintf("%s@%d", recFail.Kind, recFail.Seq)
		r.Status = recFail.Kind
		r.Detail = recFail.Message
		r.Seq = &seq
		if recFail.Kind == journal.MarkerInvariant {
			failInv = recFail.Message
		}
	}
	return rawRunData{
		run:          r,
		stepRawOuts:  rawOuts,
		stepRawInput: rawIns,
		failInvName:  failInv,
	}
}

func buildReplayRun(id, build, bin string, recSteps []rawStep, res *replay.Result, err error, maxData int, div *replay.Divergence) rawRunData {
	binLabel := replayBinName(bin)
	if err != nil {
		return rawRunData{
			run: Run{
				ID:      id,
				Mode:    "replay",
				Build:   build,
				Bin:     binLabel,
				Verdict: "error",
				Status:  "error",
				Detail:  err.Error(),
				Error:   err.Error(),
				Steps:   []Step{},
			},
		}
	}

	bySeq := make(map[uint64]replay.StepResult, len(res.Steps))
	for _, sr := range res.Steps {
		bySeq[sr.Seq] = sr
	}

	steps := make([]Step, len(recSteps))
	rawOuts := make([][]rawOut, len(recSteps))
	rawIns := make([][]byte, len(recSteps))

	for i, rs := range recSteps {
		inStr, inTrunc, omitted := previewData(rs.Data, maxData)
		st := Step{
			Seq:            rs.Seq,
			Source:         rs.Source,
			Position:       rs.Position,
			Input:          inStr,
			InputTruncated: inTrunc,
			Omitted:        omitted,
			Reads:          nonEmptyReads(rs.Reads),
			Outcome:        "not_reached",
		}
		rawIns[i] = rs.Data

		if sr, ok := bySeq[rs.Seq]; ok {
			outs := make([]Out, len(sr.Outputs))
			roList := make([]rawOut, len(sr.Outputs))
			for oi, o := range sr.Outputs {
				roList[oi] = rawOut{Sink: o.Sink, Data: o.Data}
				d, tr, _ := previewData(o.Data, maxData)
				outs[oi] = Out{Sink: o.Sink, Data: d, Truncated: tr}
			}
			st.Outputs = outs
			rawOuts[i] = roList

			switch {
			case sr.Panic != "":
				st.Outcome = "panic"
				st.Message = sr.Panic
			case sr.Error != "":
				st.Outcome = "error"
				st.Message = sr.Error
			case sr.Crash != "":
				st.Outcome = "crash"
				st.Message = sr.Crash
			case sr.Timeout != "":
				st.Outcome = "timeout"
				st.Message = sr.Timeout
			default:
				st.Outcome = "ok"
			}
		}
		steps[i] = st
	}

	if res.Seq != nil {
		targetIdx := -1
		for i := range steps {
			if steps[i].Seq <= *res.Seq {
				targetIdx = i
			}
		}
		if targetIdx >= 0 {
			switch res.Status {
			case replay.StatusInvariantViolated:
				steps[targetIdx].Outcome = "invariant"
				if res.Detail != "" {
					steps[targetIdx].Message = res.Invariant + ": " + res.Detail
				} else {
					steps[targetIdx].Message = res.Invariant
				}
			case replay.StatusNondeterministic:
				steps[targetIdx].Outcome = "nondeterministic"
				steps[targetIdx].Message = res.Detail
			case replay.StatusDiverged:
				steps[targetIdx].Outcome = "diverged"
				steps[targetIdx].Message = res.Detail
			}
		}
	}

	declared := append([]string(nil), res.Invariants...)
	r := Run{
		ID:         id,
		Mode:       "replay",
		Build:      build,
		Bin:        binLabel,
		Verdict:    res.String(),
		Status:     string(res.Status),
		Detail:     res.Detail,
		Seq:        res.Seq,
		Declared:   declared,
		Steps:      steps,
		Divergence: div,
	}
	return rawRunData{
		run:          r,
		stepRawOuts:  rawOuts,
		stepRawInput: rawIns,
		failInvName:  res.Invariant,
	}
}

func replayBinName(bin string) string {
	words, err := replay.SplitCommand(bin)
	if err == nil && len(words) > 0 {
		return filepath.Base(words[0])
	}
	return filepath.Base(bin)
}

func nonEmptyReads(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		if v > 0 {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func previewData(b []byte, maxData int) (text string, truncated bool, omitted bool) {
	if maxData == 0 {
		return "", false, true
	}
	if len(b) == 0 {
		return "", false, false
	}
	trunc := maxData > 0 && len(b) > maxData
	if utf8.Valid(b) && !bytes.ContainsAny(b, "\x00") {
		s := b
		if trunc {
			s = b[:maxData]
			for len(s) > 0 && !utf8.Valid(s) {
				s = s[:len(s)-1]
			}
		}
		out := string(s)
		if trunc {
			out += "…"
		}
		return out, trunc, false
	}
	// Binary / non-UTF-8: render as hex.
	slice := b
	if trunc {
		slice = b[:maxData]
	}
	out := fmt.Sprintf("%x", slice)
	if trunc {
		out += "…"
	}
	return out, trunc, false
}

func evalNodeRules(nodes []Node, fixtures []*fixtureData) {
	for _, fd := range fixtures {
		for ri := range fd.runs {
			rd := &fd.runs[ri]
			for si := range rd.run.Steps {
				st := &rd.run.Steps[si]
				if st.Outcome == "not_reached" {
					continue
				}
				var hits []Hit
				seen := map[hitKey]bool{}
				addHit := func(node, role string, outIdx int) {
					k := hitKey{node, role, outIdx}
					if !seen[k] {
						seen[k] = true
						hits = append(hits, Hit{Node: node, Role: role, Output: outIdx})
					}
				}

				rawIn := rd.stepRawInput[si]
				rawOuts := rd.stepRawOuts[si]

				for ni := range nodes {
					n := &nodes[ni]
					for mi := range n.Match {
						r := &n.Match[mi]
						if r.matchInput(st.Source, st.Position, rawIn) {
							addHit(n.ID, "input", -1)
						}
						for oi, ro := range rawOuts {
							if r.matchOutput(ro.Sink, ro.Data) {
								addHit(n.ID, "output", oi)
							}
						}
						if isFailureOutcome(st.Outcome) && r.matchFailure(st.Outcome, st.Message) {
							addHit(n.ID, "failure", -1)
						}
					}
				}
				st.Hits = hits
			}
		}
	}
}

type hitKey struct {
	node   string
	role   string
	output int
}

func isFailureOutcome(outcome string) bool {
	switch outcome {
	case "panic", "error", "invariant", "crash", "timeout":
		return true
	}
	return false
}

func synthesizeUnmapped(m *Model, noSpec bool, fixtures []*fixtureData) {
	existingIDs := map[string]bool{}
	specInvs := map[string]bool{}
	for _, n := range m.Nodes {
		existingIDs[n.ID] = true
		if n.Kind == "invariant" && n.Invariant != "" {
			specInvs[n.Invariant] = true
		}
	}
	uniqueNodeID := func(base string) string {
		if !existingIDs[base] {
			existingIDs[base] = true
			return base
		}
		for i := 2; ; i++ {
			cand := fmt.Sprintf("%s-%d", base, i)
			if !existingIDs[cand] {
				existingIDs[cand] = true
				return cand
			}
		}
	}

	// Collect unmapped inputs per (source, position).
	type srcPos struct{ source, position string }
	type splitInfo struct {
		field string
		val   string
	}
	unmappedInputs := map[srcPos]map[splitInfo]bool{}
	var srcPosOrder []srcPos
	unmappedSinks := map[string]bool{}
	var sinkOrder []string
	unmappedInvs := map[string]bool{}
	var invOrder []string
	unmappedFailures := map[string]bool{}
	var failOrder []string

	// Also track distinct positions per source+position to check collisions.
	distinctSourcesForPos := map[string]map[string]bool{}

	for _, fd := range fixtures {
		for ri := range fd.runs {
			rd := &fd.runs[ri]
			for _, inv := range rd.run.Declared {
				if !specInvs[inv] && !unmappedInvs[inv] {
					unmappedInvs[inv] = true
					invOrder = append(invOrder, inv)
				}
			}
			if rd.failInvName != "" && !specInvs[rd.failInvName] && !unmappedInvs[rd.failInvName] {
				unmappedInvs[rd.failInvName] = true
				invOrder = append(invOrder, rd.failInvName)
			}

			for si := range rd.run.Steps {
				st := &rd.run.Steps[si]
				if st.Outcome == "not_reached" {
					continue
				}
				hasInHit := false
				outHit := map[int]bool{}
				hasFailHit := false
				for _, h := range st.Hits {
					switch h.Role {
					case "input":
						hasInHit = true
					case "output":
						outHit[h.Output] = true
					case "failure":
						hasFailHit = true
					}
				}

				if !hasInHit {
					sp := srcPos{st.Source, st.Position}
					if unmappedInputs[sp] == nil {
						unmappedInputs[sp] = map[splitInfo]bool{}
						srcPosOrder = append(srcPosOrder, sp)
					}
					if distinctSourcesForPos[st.Position] == nil {
						distinctSourcesForPos[st.Position] = map[string]bool{}
					}
					distinctSourcesForPos[st.Position][st.Source] = true

					f, v := extractOpOrType(rd.stepRawInput[si])
					unmappedInputs[sp][splitInfo{f, v}] = true
				}

				for oi, o := range st.Outputs {
					if !outHit[oi] && !unmappedSinks[o.Sink] {
						unmappedSinks[o.Sink] = true
						sinkOrder = append(sinkOrder, o.Sink)
					}
				}

				if isFailureOutcome(st.Outcome) && st.Outcome != "invariant" && !hasFailHit {
					if !unmappedFailures[st.Outcome] {
						unmappedFailures[st.Outcome] = true
						failOrder = append(failOrder, st.Outcome)
					}
				}
			}
		}
	}

	groupFor := func(noSpecGroup string) string {
		if noSpec {
			return noSpecGroup
		}
		return "unmapped"
	}

	var synth []Node
	// 1. Input nodes
	for _, sp := range srcPosOrder {
		splits := unmappedInputs[sp]
		// Check if there are 2+ non-empty op/type values.
		var nonEmpty []splitInfo
		for k := range splits {
			if k.val != "" {
				nonEmpty = append(nonEmpty, k)
			}
		}
		sort.Slice(nonEmpty, func(i, j int) bool {
			if nonEmpty[i].field != nonEmpty[j].field {
				return nonEmpty[i].field < nonEmpty[j].field
			}
			return nonEmpty[i].val < nonEmpty[j].val
		})

		posLabel := sp.position
		if posLabel == "" {
			posLabel = sp.source
		} else if len(distinctSourcesForPos[sp.position]) > 1 && sp.source != "" {
			posLabel = sp.source + "@" + sp.position
		}

		if len(nonEmpty) >= 2 {
			for _, kv := range nonEmpty {
				id := uniqueNodeID(fmt.Sprintf("in:%s·%s=%s", posLabel, kv.field, kv.val))
				r := Rule{
					On:       "input",
					Source:   sp.source,
					Position: sp.position,
					JSON:     map[string]any{kv.field: kv.val},
				}
				_ = r.prepare()
				synth = append(synth, Node{
					ID:    id,
					Group: groupFor("inputs"),
					Kind:  "entry",
					Title: kv.val,
					Plain: fmt.Sprintf("%s @ %s (%s=%s)", sp.source, sp.position, kv.field, kv.val),
					Match: []Rule{r},
					Auto:  true,
				})
			}
			if splits[splitInfo{"", ""}] {
				id := uniqueNodeID("in:" + posLabel)
				r := Rule{On: "input", Source: sp.source, Position: sp.position}
				_ = r.prepare()
				synth = append(synth, Node{
					ID:    id,
					Group: groupFor("inputs"),
					Kind:  "entry",
					Title: posLabel,
					Plain: fmt.Sprintf("%s @ %s", sp.source, sp.position),
					Match: []Rule{r},
					Auto:  true,
				})
			}
		} else {
			id := uniqueNodeID("in:" + posLabel)
			r := Rule{On: "input", Source: sp.source, Position: sp.position}
			_ = r.prepare()
			title := posLabel
			if len(nonEmpty) == 1 && sp.position != "" && !isDigits(sp.position) {
				title = posLabel + " · " + nonEmpty[0].val
			}
			synth = append(synth, Node{
				ID:    id,
				Group: groupFor("inputs"),
				Kind:  "entry",
				Title: title,
				Plain: fmt.Sprintf("%s @ %s", sp.source, sp.position),
				Match: []Rule{r},
				Auto:  true,
			})
		}
	}

	// 2. Output / sink nodes
	for _, sink := range sinkOrder {
		id := uniqueNodeID("out:" + sink)
		r := Rule{On: "output", Sink: sink}
		_ = r.prepare()
		synth = append(synth, Node{
			ID:    id,
			Group: groupFor("outputs"),
			Kind:  "sink",
			Title: sink,
			Plain: "Emitted output sink " + sink,
			Match: []Rule{r},
			Auto:  true,
		})
	}

	// 3. Invariant nodes
	for _, inv := range invOrder {
		id := uniqueNodeID("inv:" + inv)
		synth = append(synth, Node{
			ID:        id,
			Group:     groupFor("invariants"),
			Kind:      "invariant",
			Title:     inv,
			Plain:     "Declared invariant " + inv,
			Invariant: inv,
			Auto:      true,
		})
	}

	// 4. Failure nodes
	for _, kind := range failOrder {
		id := uniqueNodeID("fail:" + kind)
		r := Rule{On: "failure", Kind: kind}
		_ = r.prepare()
		synth = append(synth, Node{
			ID:    id,
			Group: groupFor("failures"),
			Kind:  "bug",
			Title: kind,
			Plain: "Recorded or replayed " + kind + " failure",
			Match: []Rule{r},
			Auto:  true,
		})
	}

	if len(synth) == 0 {
		return
	}

	// Add missing groups needed by synthesized nodes.
	existingGroups := map[string]bool{}
	for _, g := range m.Groups {
		existingGroups[g.ID] = true
	}
	if noSpec {
		candidates := []Group{
			{ID: "inputs", Title: "Inputs", Plain: "Recorded entrypoints by source, position, and operation"},
			{ID: "outputs", Title: "Outputs", Plain: "Sinks emitted during step execution"},
			{ID: "invariants", Title: "Invariants", Plain: "Declared invariants checked after each step"},
			{ID: "failures", Title: "Failures", Plain: "Recorded or replayed failures"},
		}
		needed := map[string]bool{}
		for _, n := range synth {
			needed[n.Group] = true
		}
		for _, g := range candidates {
			if needed[g.ID] && !existingGroups[g.ID] {
				m.Groups = append(m.Groups, g)
				existingGroups[g.ID] = true
			}
		}
	} else {
		if !existingGroups["unmapped"] {
			m.Groups = append(m.Groups, Group{
				ID:    "unmapped",
				Title: "Not on the map",
				Plain: "Runtime entities seen in fixtures or replays without a matching spec card",
			})
		}
	}

	m.Nodes = append(m.Nodes, synth...)
}

func extractOpOrType(data []byte) (field, val string) {
	obj, ok := parseJSONObject(data)
	if !ok {
		return "", ""
	}
	for _, k := range []string{"op", "type"} {
		if v, exists := obj[k]; exists {
			if s, ok := v.(string); ok && s != "" {
				return k, s
			}
		}
	}
	return "", ""
}

func addAutoEdges(m *Model, noSpec bool, fixtures []*fixtureData) {
	nodeByID := make(map[string]Node, len(m.Nodes))
	invNodeByName := map[string]string{}
	for _, n := range m.Nodes {
		nodeByID[n.ID] = n
		if n.Kind == "invariant" && n.Invariant != "" && invNodeByName[n.Invariant] == "" {
			invNodeByName[n.Invariant] = n.ID
		}
	}

	type edgeKey struct{ from, to, kind string }
	counts := map[edgeKey]int{}
	var order []edgeKey
	addCount := func(from, to, kind string) {
		if !noSpec && !nodeByID[from].Auto && !nodeByID[to].Auto {
			return
		}
		k := edgeKey{from, to, kind}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}

	for _, fd := range fixtures {
		for ri := range fd.runs {
			rd := &fd.runs[ri]
			for si := range rd.run.Steps {
				st := &rd.run.Steps[si]
				if st.Outcome == "not_reached" {
					continue
				}
				var inNodes, outNodes, failNodes []string
				for _, h := range st.Hits {
					switch h.Role {
					case "input":
						inNodes = append(inNodes, h.Node)
					case "output":
						outNodes = append(outNodes, h.Node)
					case "failure":
						failNodes = append(failNodes, h.Node)
					}
				}
				for _, inID := range inNodes {
					for _, outID := range outNodes {
						addCount(inID, outID, "emits")
					}
					for _, fID := range failNodes {
						addCount(inID, fID, "breaks")
					}
					if st.Outcome == "invariant" && rd.failInvName != "" {
						if invID := invNodeByName[rd.failInvName]; invID != "" {
							addCount(inID, invID, "breaks")
						}
					}
				}
			}
		}
	}

	for _, k := range order {
		m.Edges = append(m.Edges, Edge{
			From:  k.from,
			To:    k.to,
			Kind:  k.kind,
			Auto:  true,
			Count: counts[k],
		})
	}
}

func computeInvariantsAndDetection(nodes []Node, fixtures []*fixtureData) {
	// Spec invariant names (non-auto nodes first, then auto if any, deduplicated).
	var specInvNames []string
	seenSpecInv := map[string]bool{}
	invNodesByName := map[string][]Node{}
	invNodeSet := map[string]Node{}

	for _, n := range nodes {
		if n.Kind == "invariant" && n.Invariant != "" {
			invNodeSet[n.ID] = n
			invNodesByName[n.Invariant] = append(invNodesByName[n.Invariant], n)
			if !n.Auto && !seenSpecInv[n.Invariant] {
				seenSpecInv[n.Invariant] = true
				specInvNames = append(specInvNames, n.Invariant)
			}
		}
	}

	for _, fd := range fixtures {
		for ri := range fd.runs {
			rd := &fd.runs[ri]
			r := &rd.run

			// Build Run.Invariants: declared first, then undeclared spec invariants,
			// then any failing invariant name not in either list.
			var invList []string
			seen := map[string]bool{}
			addInv := func(name string) {
				if name != "" && !seen[name] {
					seen[name] = true
					invList = append(invList, name)
				}
			}
			declaredIdx := map[string]int{}
			for _, name := range r.Declared {
				if _, ok := declaredIdx[name]; !ok {
					declaredIdx[name] = len(declaredIdx)
				}
				addInv(name)
			}
			for _, name := range specInvNames {
				addInv(name)
			}
			if rd.failInvName != "" {
				addInv(rd.failInvName)
			}
			r.Invariants = invList

			for si := range r.Steps {
				st := &r.Steps[si]

				// Add failure hit to invariant node(s) when step outcome is "invariant".
				if st.Outcome == "invariant" && rd.failInvName != "" {
					for _, invNode := range invNodesByName[rd.failInvName] {
						hasFail := false
						for _, h := range st.Hits {
							if h.Node == invNode.ID && h.Role == "failure" {
								hasFail = true
								break
							}
						}
						if !hasFail {
							st.Hits = append(st.Hits, Hit{Node: invNode.ID, Role: "failure", Output: -1})
						}
					}
				}

				if len(invList) == 0 {
					continue
				}
				invStatus := make(map[string]string, len(invList))

				if r.Mode == "recorded" {
					for _, name := range invList {
						invStatus[name] = "unknown"
					}
					if st.Outcome == "invariant" && rd.failInvName != "" {
						invStatus[rd.failInvName] = "fail"
					}
				} else {
					failIdx := -1
					if st.Outcome == "invariant" && rd.failInvName != "" {
						if idx, ok := declaredIdx[rd.failInvName]; ok {
							failIdx = idx
						}
					}
					for _, name := range invList {
						idx, isDeclared := declaredIdx[name]
						if !isDeclared {
							if st.Outcome == "invariant" && name == rd.failInvName {
								invStatus[name] = "fail"
							} else {
								invStatus[name] = "unknown"
							}
							continue
						}
						switch st.Outcome {
						case "ok", "diverged":
							invStatus[name] = "pass"
						case "invariant":
							switch {
							case failIdx >= 0 && idx < failIdx:
								invStatus[name] = "pass"
							case failIdx >= 0 && idx == failIdx:
								invStatus[name] = "fail"
							case failIdx < 0 && name == rd.failInvName:
								invStatus[name] = "fail"
							default:
								invStatus[name] = "unknown"
							}
						case "panic", "error", "crash", "timeout":
							invStatus[name] = "skipped"
						case "nondeterministic":
							invStatus[name] = "unknown"
						case "not_reached":
							invStatus[name] = "not_reached"
						default:
							invStatus[name] = "unknown"
						}
					}
				}

				// Detection via match rules on invariant nodes: note we check whether the node
				// matched via its own Match rules (before we added the synthetic invariant hit).
				detectedSet := map[string]bool{}
				for _, h := range st.Hits {
					invNode, ok := invNodeSet[h.Node]
					if !ok || len(invNode.Match) == 0 {
						continue
					}
					if nodeMatchesStep(&invNode, st, rd.stepRawInput[si], rd.stepRawOuts[si]) {
						invStatus[invNode.Invariant] = "fail"
						if !detectedSet[invNode.Invariant] {
							detectedSet[invNode.Invariant] = true
							st.Detected = append(st.Detected, invNode.Invariant)
						}
					}
				}

				st.Invariants = invStatus
			}
		}
	}
}

func nodeMatchesStep(n *Node, st *Step, rawIn []byte, rawOuts []rawOut) bool {
	if st.Outcome == "not_reached" {
		return false
	}
	for mi := range n.Match {
		r := &n.Match[mi]
		if r.matchInput(st.Source, st.Position, rawIn) {
			return true
		}
		for _, ro := range rawOuts {
			if r.matchOutput(ro.Sink, ro.Data) {
				return true
			}
		}
		if isFailureOutcome(st.Outcome) && r.matchFailure(st.Outcome, st.Message) {
			return true
		}
	}
	return false
}

func computeNodeStats(m *Model) {
	nodeByID := make(map[string]Node, len(m.Nodes))
	for _, n := range m.Nodes {
		nodeByID[n.ID] = n
	}

	for _, fix := range m.Fixtures {
		for _, run := range fix.Runs {
			for _, st := range run.Steps {
				seenHit := map[string]bool{}
				for _, h := range st.Hits {
					if seenHit[h.Node] {
						continue
					}
					seenHit[h.Node] = true
					if ns := m.Stats[h.Node]; ns != nil {
						ns.Hits = append(ns.Hits, StatRef{Fixture: fix.ID, Run: run.ID, Seq: st.Seq})
					}
				}
				for _, n := range m.Nodes {
					if n.Kind == "invariant" && n.Invariant != "" && st.Invariants[n.Invariant] == "fail" {
						if ns := m.Stats[n.ID]; ns != nil {
							ns.Violations = append(ns.Violations, StatRef{Fixture: fix.ID, Run: run.ID, Seq: st.Seq})
						}
					}
				}
			}
		}
		if fix.Diff != nil {
			for _, n := range m.Nodes {
				if n.Kind == "invariant" && n.Invariant != "" {
					if it := fix.Diff.ByInvariant[n.Invariant]; it != nil {
						if ns := m.Stats[n.ID]; ns != nil {
							ns.VariantsOld += it.Old
							ns.VariantsNew += it.New
						}
					}
				}
			}
		}
	}
}

func fallbackStageNodes(f *Fixture) []string {
	var rec *Run
	for i := range f.Runs {
		if f.Runs[i].Mode == "recorded" {
			rec = &f.Runs[i]
			break
		}
	}
	if rec == nil || len(rec.Steps) == 0 {
		return nil
	}
	target := &rec.Steps[len(rec.Steps)-1]
	for i := range rec.Steps {
		if isFailureOutcome(rec.Steps[i].Outcome) || (f.Recorded != nil && rec.Steps[i].Seq == f.Recorded.Seq) {
			target = &rec.Steps[i]
			break
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range target.Hits {
		if !seen[h.Node] {
			seen[h.Node] = true
			out = append(out, h.Node)
		}
	}
	return out
}

func resolveSinkNodes(nodes []Node, sink, text string) []string {
	var dataMatched []string
	var sinkMatched []string
	textBytes := []byte(text)

	for _, n := range nodes {
		hasSink := false
		hasData := false
		for i := range n.Match {
			r := &n.Match[i]
			if r.On == "output" && r.Sink == sink {
				hasSink = true
				if text != "" && r.matchOutput(sink, textBytes) {
					hasData = true
				}
			}
		}
		if hasData {
			dataMatched = append(dataMatched, n.ID)
		}
		if hasSink {
			sinkMatched = append(sinkMatched, n.ID)
		}
	}
	if len(dataMatched) > 0 {
		return dataMatched
	}
	return sinkMatched
}

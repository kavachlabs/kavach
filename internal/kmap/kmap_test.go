package kmap

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

func TestValidationAllErrorsJoined(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "bad.json")
	raw := `{
  "kavach_map": 99,
  "title": "bad",
  "groups": [
    {"id": "g1", "title": "G1"},
    {"id": "g1", "title": "G1 dup"}
  ],
  "nodes": [
    {"id": "n1", "group": "g1", "kind": "invariant", "title": "Inv without name"},
    {"id": "n1", "group": "missing_group", "kind": "bogus_kind", "title": "Dup", "match": [
      {"contains": "x"},
      {"on": "input", "regex": "[unclosed"}
    ]}
  ],
  "edges": [
    {"from": "n1", "to": "ghost"}
  ],
  "flows": [
    {"id": "f1", "title": "F1", "steps": [{"node": "missing_flow_node", "text": "x"}]}
  ]
}`
	if err := os.WriteFile(specPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadSpec(specPath)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	msg := err.Error()
	wants := []string{
		"kavach_map must be 1",
		`duplicate group id "g1"`,
		`duplicate node id "n1"`,
		`unknown group "missing_group"`,
		`unknown kind "bogus_kind"`,
		`invariant name is required`,
		`unknown to node "ghost"`,
		`unknown node "missing_flow_node"`,
		`rule has no "on"`,
		`invalid regex`,
	}
	for _, w := range wants {
		if !strings.Contains(msg, w) {
			t.Errorf("validation error missing %q:\n%s", w, msg)
		}
	}
}

func TestRuleMatchingAndOnInference(t *testing.T) {
	s := &Spec{
		Version: 1,
		Title:   "Rules",
		Groups:  []Group{{ID: "g", Title: "G", CodeBase: "https://example.com/repo"}},
		Nodes: []Node{
			{
				ID:    "in_glob",
				Group: "g",
				Kind:  "entry",
				Title: "Glob",
				Code:  []string{"a/b.go:10-20"},
				Match: []Rule{{
					Source:   "http",
					Position: "PUT /tx/*",
					JSON:     map[string]any{"amount": 150, "meta.kind": "commit"},
					Contains: "hotel",
					Regex:    `"amount":\s*150`,
				}},
			},
			{
				ID:    "out_sink",
				Group: "g",
				Kind:  "sink",
				Title: "Sink",
				Match: []Rule{{Sink: "ledger", Contains: "APPLIED"}},
			},
			{
				ID:    "fail_rule",
				Group: "g",
				Kind:  "bug",
				Title: "Bug",
				Match: []Rule{{Kind: "error", Regex: `insufficient.*balance`}},
			},
		},
	}
	if err := s.NormalizeAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Nodes[0].Match[0].On != "input" {
		t.Errorf("inferred on = %q, want input", s.Nodes[0].Match[0].On)
	}
	if s.Nodes[1].Match[0].On != "output" {
		t.Errorf("inferred on = %q, want output", s.Nodes[1].Match[0].On)
	}
	if s.Nodes[2].Match[0].On != "failure" {
		t.Errorf("inferred on = %q, want failure", s.Nodes[2].Match[0].On)
	}
	if len(s.Nodes[0].Links) != 1 || s.Nodes[0].Links[0].URL != "https://example.com/repo/a/b.go#L10-L20" {
		t.Errorf("unexpected links: %+v", s.Nodes[0].Links)
	}

	payload := []byte(`{"amount":150.0,"desc":"hotel","meta":{"kind":"commit"}}`)
	if !s.Nodes[0].Match[0].matchInput("http", "PUT /tx/123", payload) {
		t.Error("expected input rule to match")
	}
	if s.Nodes[0].Match[0].matchInput("http", "POST /tx/123", payload) {
		t.Error("expected non-matching position to fail")
	}
	if s.Nodes[0].Match[0].matchInput("http", "PUT /tx/123", []byte(`{"amount":151,"desc":"hotel","meta":{"kind":"commit"}}`)) {
		t.Error("expected mismatched json number to fail")
	}
	if !s.Nodes[1].Match[0].matchOutput("ledger", []byte("status=APPLIED")) {
		t.Error("expected output rule to match")
	}
	if !s.Nodes[2].Match[0].matchFailure("error", "insufficient funds in balance") {
		t.Error("expected failure rule to match")
	}
}

func TestRecordedRunLedgerFixture(t *testing.T) {
	fixPath := filepath.Join("..", "..", "examples", "ledger", "testdata", "null-amount.kavach")
	j, err := journal.ReadFile(fixPath)
	if err != nil {
		t.Fatal(err)
	}
	var inputCount int
	for _, r := range j.Records {
		if r.Type == journal.TypeInput {
			inputCount++
		}
	}

	m, err := Build(Input{
		Fixtures: []string{fixPath},
		MaxData:  512,
		BaseDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Fixtures) != 1 || len(m.Fixtures[0].Runs) != 1 {
		t.Fatalf("expected 1 fixture and 1 recorded run, got %d / %d", len(m.Fixtures), len(m.Fixtures[0].Runs))
	}
	run := m.Fixtures[0].Runs[0]
	if len(run.Steps) != inputCount {
		t.Fatalf("step count = %d, want %d", len(run.Steps), inputCount)
	}
	last := run.Steps[len(run.Steps)-1]
	if last.Outcome != "panic" || last.Seq != 32 {
		t.Fatalf("last step = (%s, seq %d), want (panic, seq 32)", last.Outcome, last.Seq)
	}
	if run.Verdict != "panic@32" {
		t.Errorf("run verdict = %q, want panic@32", run.Verdict)
	}
}

func TestInvariantSemanticsFakeReplay(t *testing.T) {
	dir := t.TempDir()
	fixPath := filepath.Join(dir, "sample.kavach")
	recs := []journal.Record{
		{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Seq: 0},
		{Type: journal.TypeInput, Seq: 1, Source: "http", Position: "POST /a", Data: []byte(`{"op":"ok"}`)},
		{Type: journal.TypeOutput, Seq: 2, Sink: "out", Data: []byte(`ok`)},
		{Type: journal.TypeInput, Seq: 3, Source: "http", Position: "POST /b", Data: []byte(`{"op":"err"}`)},
		{Type: journal.TypeOutput, Seq: 4, Sink: "out", Data: []byte(`{"invariant":"undeclared_rule","bad":true}`)},
		{Type: journal.TypeMarker, Seq: 5, Kind: journal.MarkerError, Message: "boom"},
		{Type: journal.TypeInput, Seq: 6, Source: "http", Position: "POST /c", Data: []byte(`{"op":"inv"}`)},
		{Type: journal.TypeInput, Seq: 7, Source: "http", Position: "POST /d", Data: []byte(`{"op":"after"}`)},
	}
	if err := journal.WriteFile(fixPath, journal.Meta{Service: "svc", Start: "genesis"}, recs); err != nil {
		t.Fatal(err)
	}

	spec := &Spec{
		Version: 1,
		Title:   "Inv Test",
		Groups:  []Group{{ID: "g", Title: "G"}},
		Nodes: []Node{
			{ID: "inv_A", Group: "g", Kind: "invariant", Title: "A", Invariant: "A"},
			{ID: "inv_X", Group: "g", Kind: "invariant", Title: "X", Invariant: "X"},
			{ID: "inv_B", Group: "g", Kind: "invariant", Title: "B", Invariant: "B"},
			{
				ID:        "inv_U",
				Group:     "g",
				Kind:      "invariant",
				Title:     "U",
				Invariant: "undeclared_rule",
				Match:     []Rule{{On: "output", Sink: "out", JSON: map[string]any{"invariant": "undeclared_rule"}}},
			},
			{ID: "inv_Unmatched", Group: "g", Kind: "invariant", Title: "Unmatched", Invariant: "undeclared_silent"},
		},
	}

	fake := func(_ *journal.Journal, bin string) (*replay.Result, error) {
		seq6 := uint64(6)
		seq1 := uint64(1)
		if bin == "old" {
			return &replay.Result{
				Status:     replay.StatusInvariantViolated,
				Seq:        &seq6,
				Invariant:  "X",
				Detail:     "X broke",
				Service:    "svc",
				Invariants: []string{"A", "X", "B"},
				Steps: []replay.StepResult{
					{Seq: 1, Outputs: []kavach.Output{{Sink: "out", Data: []byte("ok")}}},
					{Seq: 3, Outputs: []kavach.Output{{Sink: "out", Data: []byte(`{"invariant":"undeclared_rule","bad":true}`)}}, Error: "boom"},
					{Seq: 6},
				},
			}, nil
		}
		return &replay.Result{
			Status:     replay.StatusDiverged,
			Seq:        &seq1,
			Detail:     "output changed",
			Service:    "svc",
			Invariants: []string{"A", "X", "B"},
			Steps: []replay.StepResult{
				{Seq: 1, Outputs: []kavach.Output{{Sink: "out", Data: []byte("different")}}},
			},
		}, nil
	}

	m, err := Build(Input{
		Spec:     spec,
		Fixtures: []string{fixPath},
		Bin:      "old",
		New:      "new",
		MaxData:  256,
		BaseDir:  dir,
		Replay:   fake,
	})
	if err != nil {
		t.Fatal(err)
	}

	var oldRun, newRun *Run
	for i := range m.Fixtures[0].Runs {
		rn := &m.Fixtures[0].Runs[i]
		switch rn.ID {
		case "old":
			oldRun = rn
		case "new":
			newRun = rn
		}
	}
	if oldRun == nil || newRun == nil {
		t.Fatal("missing old/new replay runs")
	}

	// Step seq 3: error -> declared A/X/B are skipped; undeclared_rule detected -> fail; undeclared_silent -> unknown.
	s3 := oldRun.Steps[1]
	if s3.Outcome != "error" {
		t.Fatalf("seq 3 outcome = %q, want error", s3.Outcome)
	}
	for _, name := range []string{"A", "X", "B"} {
		if s3.Invariants[name] != "skipped" {
			t.Errorf("seq 3 inv %s = %q, want skipped", name, s3.Invariants[name])
		}
	}
	if s3.Invariants["undeclared_rule"] != "fail" {
		t.Errorf("seq 3 undeclared_rule = %q, want fail", s3.Invariants["undeclared_rule"])
	}
	if len(s3.Detected) != 1 || s3.Detected[0] != "undeclared_rule" {
		t.Errorf("seq 3 detected = %v, want [undeclared_rule]", s3.Detected)
	}
	if s3.Invariants["undeclared_silent"] != "unknown" {
		t.Errorf("seq 3 undeclared_silent = %q, want unknown", s3.Invariants["undeclared_silent"])
	}

	// Step seq 6: invariant_violated(X) with declared [A, X, B] -> A=pass, X=fail, B=unknown.
	s6 := oldRun.Steps[2]
	if s6.Invariants["A"] != "pass" || s6.Invariants["X"] != "fail" || s6.Invariants["B"] != "unknown" {
		t.Errorf("seq 6 invariants = %+v, want A=pass X=fail B=unknown", s6.Invariants)
	}

	// Step seq 7: after stop -> not_reached.
	s7 := oldRun.Steps[3]
	if s7.Outcome != "not_reached" || s7.Invariants["A"] != "not_reached" {
		t.Errorf("seq 7 = (%s, %+v), want not_reached", s7.Outcome, s7.Invariants)
	}

	// New run seq 1: diverged -> declared invariants checked before diff, so pass.
	ns1 := newRun.Steps[0]
	if ns1.Outcome != "diverged" || ns1.Invariants["A"] != "pass" || ns1.Invariants["X"] != "pass" {
		t.Errorf("new seq 1 = (%s, %+v), want diverged with pass", ns1.Outcome, ns1.Invariants)
	}
}

func TestDiffImportAndBasenameAssociation(t *testing.T) {
	fixPath := filepath.Join("..", "..", "examples", "ledger", "testdata", "null-amount.kavach")
	diffPath := filepath.Join("testdata", "diff.json")
	unmatchedDiff := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(unmatchedDiff, []byte(`{"fixture":"/x/missing.kavach","verdict":"fixed"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	spec := &Spec{
		Version: 1,
		Title:   "Diff",
		Groups:  []Group{{ID: "g", Title: "G"}},
		Nodes: []Node{
			{ID: "inv_bal", Group: "g", Kind: "invariant", Title: "bal", Invariant: "balances_non_negative"},
		},
	}

	m, err := Build(Input{
		Spec:     spec,
		Fixtures: []string{fixPath},
		Diffs:    []string{diffPath, unmatchedDiff},
		MaxData:  128,
		BaseDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Fixtures[0].Diff == nil {
		t.Fatal("expected diff to match null-amount.kavach by basename")
	}
	d := m.Fixtures[0].Diff
	if d.ByKind["field"].Passed != 1 || d.ByKind["field"].Total != 1 {
		t.Errorf("field tally = %+v", d.ByKind["field"])
	}
	if d.ByKind["pair"].Total != 1 || d.ByKind["move"].Total != 1 || d.ByKind["drop"].Total != 1 || d.ByKind["clock"].Total != 1 {
		t.Errorf("unexpected by_kind: %+v", d.ByKind)
	}
	if d.ByInvariant["balances_non_negative"].Old != 2 || d.ByInvariant["balances_non_negative"].New != 2 {
		t.Errorf("balances_non_negative tally = %+v", d.ByInvariant["balances_non_negative"])
	}
	if d.ByInvariant["money_conserved"].New != 2 {
		t.Errorf("money_conserved tally = %+v", d.ByInvariant["money_conserved"])
	}
	if m.Stats["inv_bal"].VariantsOld != 2 || m.Stats["inv_bal"].VariantsNew != 2 {
		t.Errorf("node stats variants = %d/%d", m.Stats["inv_bal"].VariantsOld, m.Stats["inv_bal"].VariantsNew)
	}
	if len(m.Warnings) != 1 || !strings.Contains(m.Warnings[0], "matched no fixture") {
		t.Errorf("expected unmatched diff warning, got %v", m.Warnings)
	}
}

func TestOverlayRelativeFixtureAndUnknownNodeWarning(t *testing.T) {
	dir := t.TempDir()
	ovDir := filepath.Join(dir, "ov")
	capDir := filepath.Join(dir, "cap")
	if err := os.MkdirAll(ovDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixPath := filepath.Join(capDir, "inc.kavach")
	if err := journal.WriteFile(fixPath, journal.Meta{Service: "s", Start: "genesis"}, []journal.Record{
		{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Seq: 0},
		{Type: journal.TypeInput, Seq: 1, Source: "http", Position: "POST /x", Data: []byte(`{}`)},
		{Type: journal.TypeOutput, Seq: 2, Sink: "ledger", Data: []byte(`ok`)},
	}); err != nil {
		t.Fatal(err)
	}
	ovPath := filepath.Join(ovDir, "shadow.json")
	ovJSON := `{
  "kavach_map_overlay": 1,
  "mode": "shadow",
  "title": "Shadow",
  "lanes": [{
    "id": "l1",
    "title": "L1",
    "stages": [{
      "id": "s1",
      "title": "S1",
      "status": "pass",
      "nodes": ["known_node", "ghost_node"],
      "fixture": "../cap/inc.kavach",
      "items": [{"label": "apply", "sink": "ledger", "text": "ok"}]
    }]
  }]
}`
	if err := os.WriteFile(ovPath, []byte(ovJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &Spec{
		Version: 1,
		Title:   "Ov",
		Groups:  []Group{{ID: "g", Title: "G"}},
		Nodes: []Node{
			{ID: "known_node", Group: "g", Kind: "sink", Title: "Ledger", Match: []Rule{{On: "output", Sink: "ledger"}}},
		},
	}

	m, err := Build(Input{
		Spec:     spec,
		Overlays: []string{ovPath},
		MaxData:  256,
		BaseDir:  dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Fixtures) != 1 || m.Fixtures[0].Origin != "overlay" {
		t.Fatalf("expected 1 overlay fixture, got %+v", m.Fixtures)
	}
	st := m.Overlays[0].Lanes[0].Stages[0]
	if st.Fixture != "cap/inc.kavach" {
		t.Errorf("stage fixture = %q, want cap/inc.kavach", st.Fixture)
	}
	if len(st.Nodes) != 1 || st.Nodes[0] != "known_node" {
		t.Errorf("stage nodes = %v, want [known_node]", st.Nodes)
	}
	if len(st.Items[0].Nodes) != 1 || st.Items[0].Nodes[0] != "known_node" {
		t.Errorf("item nodes = %v, want [known_node]", st.Items[0].Nodes)
	}
	if len(m.Warnings) != 1 || !strings.Contains(m.Warnings[0], "ghost_node") {
		t.Errorf("expected ghost_node warning, got %v", m.Warnings)
	}
}

func TestRenderHTMLXSSSafeAndRoundTripsJSON(t *testing.T) {
	dir := t.TempDir()
	fixPath := filepath.Join(dir, "xss.kavach")
	evil := `</script><img src=x onerror=alert(1)>`
	if err := journal.WriteFile(fixPath, journal.Meta{Service: "xss", Start: "genesis"}, []journal.Record{
		{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Seq: 0},
		{Type: journal.TypeInput, Seq: 1, Source: "http", Position: "POST /xss", Data: []byte(evil)},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := Build(Input{
		Fixtures: []string{fixPath},
		MaxData:  512,
		BaseDir:  dir,
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, m); err != nil {
		t.Fatal(err)
	}
	htmlStr := buf.String()
	if strings.Contains(htmlStr, evil) {
		t.Fatal("rendered HTML contains unescaped script breakout payload")
	}
	marker := `<script type="application/json" id="kavach-map-data">`
	idx := strings.Index(htmlStr, marker)
	if idx < 0 {
		t.Fatal("rendered HTML missing id=\"kavach-map-data\"")
	}
	rest := htmlStr[idx+len(marker):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatal("missing closing script tag for kavach-map-data")
	}
	var decoded Model
	if err := json.Unmarshal([]byte(rest[:end]), &decoded); err != nil {
		t.Fatalf("unmarshal embedded model: %v", err)
	}
	if decoded.Fixtures[0].Runs[0].Steps[0].Input != evil {
		t.Errorf("decoded input = %q, want %q", decoded.Fixtures[0].Runs[0].Steps[0].Input, evil)
	}
}

func TestAutoSpecWithoutSpecFile(t *testing.T) {
	fixPath := filepath.Join("..", "..", "examples", "ledger", "testdata", "null-amount.kavach")
	m, err := Build(Input{
		Fixtures: []string{fixPath},
		MaxData:  128,
		BaseDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var hasIn, hasOut, hasFail bool
	for _, n := range m.Nodes {
		if !n.Auto {
			t.Errorf("node %s should have Auto=true", n.ID)
		}
		if strings.HasPrefix(n.ID, "in:") {
			hasIn = true
		}
		if strings.HasPrefix(n.ID, "out:") {
			hasOut = true
		}
		if strings.HasPrefix(n.ID, "fail:") {
			hasFail = true
		}
	}
	if !hasIn || !hasOut || !hasFail {
		t.Errorf("missing auto nodes: in=%v out=%v fail=%v", hasIn, hasOut, hasFail)
	}
	if len(m.Edges) == 0 {
		t.Error("expected auto edges")
	}
}

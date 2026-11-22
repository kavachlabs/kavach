package kmap

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// deckModel builds a small model with a recorded run plus old and new builds
// that agree on seq 1 and diverge on seq 3 (old errors, new is ok).
func deckModel(t *testing.T) *Model {
	t.Helper()
	dir := t.TempDir()
	fixPath := filepath.Join(dir, "deck.kavach")
	recs := []journal.Record{
		{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Seq: 0},
		{Type: journal.TypeInput, Seq: 1, Source: "http", Position: "POST /pay", Data: []byte(`{"amt":5}`)},
		{Type: journal.TypeOutput, Seq: 2, Sink: "ledger", Data: []byte(`{"amt":5}`)},
		{Type: journal.TypeInput, Seq: 3, Source: "http", Position: "POST /pay", Data: []byte(`{"amt":7}`)},
		{Type: journal.TypeMarker, Seq: 4, Kind: journal.MarkerError, Message: "boom"},
	}
	if err := journal.WriteFile(fixPath, journal.Meta{Service: "svc", Start: "genesis"}, recs); err != nil {
		t.Fatal(err)
	}
	spec := &Spec{
		Version: 1,
		Title:   "Deck Test",
		Groups:  []Group{{ID: "g", Title: "G"}},
		Nodes: []Node{
			{ID: "in_pay", Group: "g", Kind: "entry", Title: "POST /pay", Match: []Rule{{On: "input", Position: "POST /pay"}}},
			{ID: "fn_pay", Group: "g", Kind: "function", Title: "Pay"},
			{ID: "bug_1", Group: "g", Kind: "bug", Title: "B1: boom", Match: []Rule{{On: "failure", Contains: "boom"}}},
			{ID: "sink_ledger", Group: "g", Kind: "sink", Title: "ledger", Match: []Rule{{On: "output", Sink: "ledger"}}},
		},
		Edges: []Edge{
			{From: "in_pay", To: "fn_pay", Kind: "calls"},
			{From: "fn_pay", To: "bug_1", Kind: "calls"},
			{From: "fn_pay", To: "sink_ledger", Kind: "writes"},
		},
	}
	fake := func(_ *journal.Journal, bin string) (*replay.Result, error) {
		seq3 := uint64(3)
		if bin == "old" {
			return &replay.Result{
				Status: replay.StatusStillFailing, Seq: &seq3, Detail: "boom", Service: "svc",
				Steps: []replay.StepResult{
					{Seq: 1, Outputs: []kavach.Output{{Sink: "ledger", Data: []byte(`{"amt":5}`)}}},
					{Seq: 3, Error: "boom"},
				},
			}, nil
		}
		return &replay.Result{
			Status: replay.StatusOK, Service: "svc",
			Steps: []replay.StepResult{
				{Seq: 1, Outputs: []kavach.Output{{Sink: "ledger", Data: []byte(`{"amt":5}`)}}},
				{Seq: 3, Outputs: []kavach.Output{{Sink: "ledger", Data: []byte(`{"amt":7}`)}}},
			},
		}, nil
	}
	m, err := Build(Input{Spec: spec, Fixtures: []string{fixPath}, Bin: "old", New: "new", MaxData: 256, BaseDir: dir, Replay: fake})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRenderHTMLNoHTMLStringsOrNetwork guards the self-contained, DOM-only
// contract of the rendered page, including the spliced replay deck.
func TestRenderHTMLNoHTMLStringsOrNetwork(t *testing.T) {
	m := deckModel(t)
	for name, render := range map[string]func(*bytes.Buffer) error{
		"static": func(b *bytes.Buffer) error { return RenderHTML(b, m) },
		"live":   func(b *bytes.Buffer) error { return RenderLiveHTML(b, m) },
	} {
		var buf bytes.Buffer
		if err := render(&buf); err != nil {
			t.Fatal(err)
		}
		page := buf.String()
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "<link", "@import", "@font-face"} {
			if strings.Contains(page, bad) {
				t.Errorf("%s: rendered HTML contains %q", name, bad)
			}
		}
		if regexp.MustCompile(`<script[^>]*\bsrc\s*=`).MatchString(page) {
			t.Errorf("%s: rendered HTML loads an external script", name)
		}
		allowed := map[string]bool{"http://www.w3.org/2000/svg": true}
		for _, u := range regexp.MustCompile(`https?://[^\s"'<>)]+`).FindAllString(page, -1) {
			if !allowed[u] {
				t.Errorf("%s: rendered HTML references URL %q", name, u)
			}
		}
		for _, ph := range []string{"{{KAVACH_MAP_TITLE}}", "/*KAVACH_MAP_CSS*/", "{{KAVACH_MAP_DATA}}", "/*KAVACH_MAP_DECK_JS*/", "/*KAVACH_MAP_JS*/"} {
			if strings.Contains(page, ph) {
				t.Errorf("%s: placeholder %s left in output", name, ph)
			}
		}
		if !strings.Contains(page, "KavachDeck") || !strings.Contains(page, ".kd-tape") {
			t.Errorf("%s: replay deck JS or CSS not spliced", name)
		}
	}
}

// TestDeckDerivationNode runs the deck's pure derivation in node against a
// real Model: steps, divergence and Model-derived hops. Skipped without node.
func TestDeckDerivationNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	m := deckModel(t)
	dir := t.TempDir()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(dir, "model.json")
	if err := os.WriteFile(modelPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	deckPath, err := filepath.Abs(filepath.Join("assets", "deck.js"))
	if err != nil {
		t.Fatal(err)
	}
	script := `
globalThis.window = globalThis;
require(process.argv[1]);
const model = JSON.parse(require("fs").readFileSync(process.argv[2], "utf8"));
const D = KavachDeck.deriveFixture(model, model.fixtures[0]);
const view = v => v ? { outcome: v.step.outcome, bad: v.bad, hops: v.hops.map(h => ({ from: h.from, to: h.to, recorded: h.recorded, bug: h.bug })) } : null;
console.log(JSON.stringify({
  firstDivStep: D.firstDivStep,
  steps: D.steps.map(s => ({ seq: s.seq, diverge: s.diverge, reasons: s.reasons, old: view(s.views.old), new: view(s.views.new) })),
}));`
	out, err := exec.Command(node, "-e", script, deckPath, modelPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	type hop struct {
		From     string   `json:"from"`
		To       []string `json:"to"`
		Recorded bool     `json:"recorded"`
		Bug      string   `json:"bug"`
	}
	type view struct {
		Outcome string `json:"outcome"`
		Bad     bool   `json:"bad"`
		Hops    []hop  `json:"hops"`
	}
	var got struct {
		FirstDivStep int `json:"firstDivStep"`
		Steps        []struct {
			Seq     uint64   `json:"seq"`
			Diverge bool     `json:"diverge"`
			Reasons []string `json:"reasons"`
			Old     *view    `json:"old"`
			New     *view    `json:"new"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode node output: %v\n%s", err, out)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("steps = %d, want 2\n%s", len(got.Steps), out)
	}
	if got.Steps[0].Diverge {
		t.Errorf("seq 1 should not diverge: %v", got.Steps[0].Reasons)
	}
	s := got.Steps[1]
	if got.FirstDivStep != 1 || s.Seq != 3 || !s.Diverge {
		t.Fatalf("first divergence = step %d (seq %d, diverge %v), want step 1 seq 3\n%s", got.FirstDivStep, s.Seq, s.Diverge, out)
	}
	if s.Old == nil || s.New == nil || !s.Old.Bad || s.New.Bad {
		t.Fatalf("seq 3: old should be bad and new ok\n%s", out)
	}
	if len(s.Old.Hops) == 0 || s.Old.Hops[0].From != "in_pay" || !s.Old.Hops[0].Recorded {
		t.Errorf("old hops should start at recorded entry in_pay: %+v", s.Old.Hops)
	}
	var sawBug, sawInferred bool
	for _, h := range s.Old.Hops {
		for _, to := range h.To {
			if to == "bug_1" {
				sawBug = true
			}
		}
		if !h.Recorded {
			sawInferred = true
		}
	}
	if !sawBug || !sawInferred {
		t.Errorf("old hops should reach bug_1 via an inferred spec hop: %+v", s.Old.Hops)
	}
	for _, h := range s.New.Hops {
		for _, to := range h.To {
			if to == "bug_1" {
				t.Errorf("new build should not reach bug_1: %+v", s.New.Hops)
			}
		}
	}
}

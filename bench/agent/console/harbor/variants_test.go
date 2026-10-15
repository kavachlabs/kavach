package harbor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
)

// variantTrial writes a trial with a collected contract: two rounds, a
// variant of round 2 with its new-build result, one of round 1 without, and
// the contract's traffic.
func variantTrial(t *testing.T) *Reader {
	t.Helper()
	jobs := t.TempDir()
	trial := filepath.Join(jobs, "job", "trial")
	cdir := filepath.Join(trial, filepath.FromSlash(contractSubdir))
	for _, d := range []string{filepath.Join(cdir, "rounds", "1"), filepath.Join(cdir, "rounds", "2")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(trial, "config.json"), `{}`)
	write(filepath.Join(cdir, "contract.json"), `{"spec":"task.md"}`)
	write(filepath.Join(cdir, "rounds.jsonl"),
		`{"round":1,"at":"2026-10-12T15:00:00Z","build":"python -m driver","verdict":"variant_failed(3)@4","changed":2,"signature":"a"}`+"\n"+
			"not json\n"+
			`{"round":2,"at":"2026-10-12T15:10:00Z","verdict":"variant_failed(12)@5","changed":2,"signature":"b"}`+"\n")

	env := journal.Record{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Seq: 0, Facts: []journal.Fact{{Key: "os", Value: []byte("linux")}}}
	traffic := []journal.Record{
		env,
		{Type: journal.TypeInput, Seq: 1, Source: "traffic", Position: "1", Data: []byte(`{"client":"c2","open":{}}`)},
		{Type: journal.TypeClock, Seq: 2, UnixNanos: 1700000000000000000},
		{Type: journal.TypeOutput, Seq: 3, Sink: "client:c2", Data: []byte(`{"ok":true}`)},
		{Type: journal.TypeInput, Seq: 4, Source: "traffic", Position: "2", Data: []byte(`{"client":"c2","url":"https://a.example/"}`)},
		{Type: journal.TypeClock, Seq: 5, UnixNanos: 1700000000000000100},
		{Type: journal.TypeOutput, Seq: 6, Sink: "client:c2", Data: []byte(`{"names":["a"]}`)},
	}
	if err := journal.WriteFile(filepath.Join(cdir, "traffic.kavach"), journal.Meta{Service: "httpx-cookies", Start: journal.StartGenesis}, traffic); err != nil {
		t.Fatal(err)
	}

	meta := journal.Meta{Service: "httpx-cookies", Start: journal.StartGenesis, Variant: &journal.Variant{ID: 12, Mutation: "input seq 1 delivered twice", Incident: 5, Failure: "invariant: cookie-limits"}}
	long := strings.Repeat("x", 5000)
	recs := []journal.Record{
		env,
		{Type: journal.TypeInput, Seq: 1, Source: "traffic", Position: "1", Data: []byte(`{"client":"c2","open":{}}`)},
		{Type: journal.TypeClock, Seq: 2, UnixNanos: 1700000000000000000},
		{Type: journal.TypeRand, Seq: 3, Data: []byte{1, 2, 3, 4}},
		{Type: journal.TypeInput, Seq: 4, Source: "traffic", Position: "1", Data: []byte{0xff, 0xfe, 'x'}},
		{Type: journal.TypeGateway, Flags: journal.FlagCritical, Seq: 5, Gateway: "db", Request: []byte("get"), Response: []byte(long)},
		{Type: journal.TypeConfig, Flags: journal.FlagCritical, Seq: 6, Key: "MAX_COOKIES", Present: true, Value: []byte("50"), Source: "env"},
		{Type: journal.TypeConfig, Flags: journal.FlagCritical, Seq: 7, Key: "STRICT"},
		{Type: journal.TypeInput, Seq: 8, Source: "traffic", Position: "2", Data: []byte(`{"client":"c2","url":"https://a.example/"}`)},
		{Type: journal.TypeOutput, Seq: 9, Sink: "client:c2", Data: []byte(`{"names":["a"]}`), Scope: journal.ScopeLocal},
		{Type: journal.TypeMarker, Seq: 10, Kind: journal.MarkerInvariant, Message: "cookie-limits"},
	}
	v12 := filepath.Join(cdir, "rounds", "2", "variant-12.kavach")
	if err := journal.WriteFile(v12, meta, recs); err != nil {
		t.Fatal(err)
	}
	write(strings.TrimSuffix(v12, ".kavach")+".json", `{"status":"invariant_violated","seq":8,"invariant":"cookie-limits",
		"detail":"client c2: holds 51 cookies, max_cookies=50","service":"httpx-cookies","start":"genesis","records":11,
		"variant":"input seq 1 delivered twice","steps":[
		{"seq":1,"outputs":[{"sink":"client:c2","data":"eyJvayI6dHJ1ZX0="}]},
		{"seq":4,"outputs":[],"error":"bad json","synthesized_reads":1},
		{"seq":8,"outputs":[{"sink":"client:c2","data":"//4="}]}]}`)

	meta.Variant = &journal.Variant{ID: 3, Mutation: "clock shifted 1h", Incident: 1, Failure: "invariant: cookie-scope"}
	if err := journal.WriteFile(filepath.Join(cdir, "rounds", "1", "variant-03.kavach"), meta, recs[:3]); err != nil {
		t.Fatal(err)
	}
	// Not variants: a non-canonical round, a stray file and a broken journal.
	os.MkdirAll(filepath.Join(cdir, "rounds", "02"), 0o755)
	write(filepath.Join(cdir, "rounds", "02", "variant-01.kavach"), "x")
	write(filepath.Join(cdir, "rounds", "1", "notes.txt"), "x")
	write(filepath.Join(cdir, "rounds", "1", "variant-07.kavach"), "not a journal")
	return &Reader{Dir: jobs}
}

func TestTrialVariants(t *testing.T) {
	r := variantTrial(t)
	l, err := r.TrialVariants("job", "trial")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Rounds) != 2 || l.Rounds[0].Round != 1 || l.Rounds[0].Build != "python -m driver" || l.Rounds[1].Verdict != "variant_failed(12)@5" || l.Rounds[1].At != "2026-10-12T15:10:00Z" || l.Note != "" {
		t.Fatalf("rounds %+v note %q", l.Rounds, l.Note)
	}
	if len(l.Variants) != 3 {
		t.Fatalf("variants %+v", l.Variants)
	}
	v3, v7, v12 := l.Variants[0], l.Variants[1], l.Variants[2]
	if v3.Round != 1 || v3.ID != 3 || v3.HasResult || v3.Variant == nil || v3.Variant.Mutation != "clock shifted 1h" || v3.Error != "" {
		t.Errorf("variant 3: %+v", v3)
	}
	if v7.Round != 1 || v7.ID != 7 || v7.Error == "" || v7.Variant != nil {
		t.Errorf("variant 7: %+v", v7)
	}
	if v12.Round != 2 || v12.ID != 12 || !v12.HasResult || v12.Verdict != "invariant_violated(cookie-limits)@8" || v12.Service != "httpx-cookies" ||
		*v12.Variant != (journal.Variant{ID: 12, Mutation: "input seq 1 delivered twice", Incident: 5, Failure: "invariant: cookie-limits"}) {
		t.Errorf("variant 12: %+v %+v", v12, v12.Variant)
	}
	b, _ := json.Marshal(v12)
	if want := `"variant":{"id":12,"mutation":"input seq 1 delivered twice","incident":5,"failure":"invariant: cookie-limits"}`; !strings.Contains(string(b), want) {
		t.Errorf("json %s", b)
	}

	// A trial without a collected contract has no variants, and says so.
	os.MkdirAll(filepath.Join(r.Dir, "job", "bare"), 0o755)
	os.WriteFile(filepath.Join(r.Dir, "job", "bare", "config.json"), []byte(`{}`), 0o644)
	l, err = r.TrialVariants("job", "bare")
	if err != nil || len(l.Variants) != 0 || len(l.Rounds) != 0 || l.Note == "" {
		t.Fatalf("%+v %v", l, err)
	}
	if b, _ := json.Marshal(l); !strings.Contains(string(b), `"rounds":[],"variants":[]`) {
		t.Errorf("json %s", b)
	}
	for _, id := range []string{"nope", "..", "../job"} {
		if _, err := r.TrialVariants("job", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestTrialVariant(t *testing.T) {
	r := variantTrial(t)
	v, err := r.TrialVariant("job", "trial", 2, 12)
	if err != nil {
		t.Fatal(err)
	}
	if v.Round != 2 || v.ID != 12 || v.Service != "httpx-cookies" || v.Start != "genesis" || v.Records != 11 || v.Truncated || v.Variant.Incident != 5 {
		t.Fatalf("%+v", v)
	}
	if len(v.Steps) != 3 {
		t.Fatalf("steps %+v", v.Steps)
	}
	s0, s1, s2 := v.Steps[0], v.Steps[1], v.Steps[2]
	if s0.Seq != 1 || s0.Source != "traffic" || s0.Position != "1" || s0.Input != `{"client":"c2","open":{}}` || s0.InputBase64 ||
		len(s0.Clock) != 1 || s0.Clock[0] != 1700000000000000000 || s0.Random != 1 || len(s0.Recorded) != 0 || s0.Marker != nil {
		t.Errorf("step 0: %+v", s0)
	}
	if s0.TrafficSeq == nil || *s0.TrafficSeq != 1 || len(s0.Traffic) != 1 || s0.Traffic[0].Data != `{"ok":true}` || s0.InputChanged {
		t.Errorf("step 0 traffic: %+v", s0)
	}
	if s1.Input != "//54" || !s1.InputBase64 || !s1.InputChanged || len(s1.Gateways) != 1 || len(s1.Config) != 2 {
		t.Errorf("step 1: %+v", s1)
	}
	g := s1.Gateways[0]
	if g.Gateway != "db" || g.Request != "get" || len(g.Response) != maxVariantText+len("…") || !strings.HasSuffix(g.Response, "…") || g.ResponseBase64 {
		t.Errorf("gateway %+v", g)
	}
	if c := s1.Config; c[0] != (VariantConfig{Key: "MAX_COOKIES", Value: "50", Present: true, Source: "env"}) || c[1] != (VariantConfig{Key: "STRICT"}) {
		t.Errorf("config %+v", c)
	}
	if s2.Seq != 8 || len(s2.Recorded) != 1 || s2.Recorded[0] != (VariantOutput{Sink: "client:c2", Data: `{"names":["a"]}`, Local: true}) ||
		s2.Marker == nil || *s2.Marker != (VariantMarker{Kind: "invariant", Message: "cookie-limits"}) {
		t.Errorf("step 2: %+v", s2)
	}

	n := v.New
	if n == nil || n.Verdict != "invariant_violated(cookie-limits)@8" || n.Status != "invariant_violated" || n.Seq == nil || *n.Seq != 8 ||
		n.Invariant != "cookie-limits" || n.Detail != "client c2: holds 51 cookies, max_cookies=50" || len(n.Steps) != 3 {
		t.Fatalf("new %+v", n)
	}
	if o := n.Steps[0].Outputs; len(o) != 1 || o[0] != (VariantOutput{Sink: "client:c2", Data: `{"ok":true}`}) {
		t.Errorf("new step 0 %+v", o)
	}
	if s := n.Steps[1]; s.Error != "bad json" || s.Synthesized != 1 || s.Outputs == nil {
		t.Errorf("new step 1 %+v", s)
	}
	if o := n.Steps[2].Outputs[0]; o.Data != "//4=" || !o.DataBase64 {
		t.Errorf("new step 2 %+v", o)
	}
	b, _ := json.Marshal(v)
	for _, want := range []string{`"input_base64":false`, `"clock":[1700000000000000000]`, `"gateways":[]`, `"panic":""`, `"error":""`, `"new":{"verdict":`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json lacks %s", want)
		}
	}

	// Without its result, new is null.
	v, err = r.TrialVariant("job", "trial", 1, 3)
	if err != nil || v.New != nil || len(v.Steps) != 1 {
		t.Fatalf("%+v %v", v, err)
	}
	if b, _ := json.Marshal(v); !strings.Contains(string(b), `"new":null`) {
		t.Errorf("json %s", b)
	}
	if _, err := r.TrialVariant("job", "trial", 1, 7); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("broken journal: %v", err)
	}
	for _, c := range []struct {
		job, id    string
		round, vid int
	}{{"job", "trial", 2, 3}, {"job", "trial", 9, 12}, {"job", "trial", -1, 12}, {"job", "trial", 2, -12}, {"..", "trial", 2, 12}, {"job", "../job/trial", 2, 12}} {
		if _, err := r.TrialVariant(c.job, c.id, c.round, c.vid); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestVariantStepsCap(t *testing.T) {
	recs := []journal.Record{{Type: journal.TypeEnvironment, Flags: journal.FlagCritical}}
	for i := 0; i < maxVariantSteps+5; i++ {
		recs = append(recs, journal.Record{Type: journal.TypeInput, Seq: uint64(i + 1), Source: "s"})
	}
	steps, cut := variantSteps(recs)
	if len(steps) != maxVariantSteps || !cut {
		t.Fatalf("%d %v", len(steps), cut)
	}
}

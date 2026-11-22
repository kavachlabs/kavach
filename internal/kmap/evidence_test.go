package kmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func evidenceSpec(nodes ...Node) *Spec {
	return &Spec{
		Version: 1,
		Title:   "Evidence",
		Groups:  []Group{{ID: "up", Title: "Upstream"}, {ID: "bugs", Title: "Bugs"}},
		Nodes:   nodes,
	}
}

func TestExcerptRangesContextAndHighlight(t *testing.T) {
	root := filepath.Join(t.TempDir(), "upstream")
	writeFile(t, filepath.Join(root, "model", "tx.go"), numberedLines(50))
	nodes := []Node{
		{ID: "fn", Group: "up", Kind: "function", Code: []string{"model/tx.go:10-12", "model/tx.go:2", "model/tx.go", "fix.patch"}},
		// A bug-group ref carrying the root's directory name still resolves.
		{ID: "bug", Group: "bugs", Kind: "bug", Code: []string{"upstream/model/tx.go:50"}},
	}
	ev, _, warns, err := attachEvidence(nodes, []SourceRoot{{Group: "up", Dir: root}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	ex := ev["fn"].Excerpts
	if len(ex) != 2 {
		t.Fatalf("fn excerpts = %d, want 2 (refs without lines are skipped)", len(ex))
	}
	e := ex[0]
	if e.Path != "model/tx.go" || e.Root != "up" || e.Start != 7 || e.HLStart != 10 || e.HLEnd != 12 || len(e.Lines) != 9 {
		t.Errorf("range excerpt = %+v", e)
	}
	if e.Lines[0] != "line 7" || e.Lines[8] != "line 15" {
		t.Errorf("range excerpt lines = %q", e.Lines)
	}
	if e := ex[1]; e.Start != 1 || e.HLStart != 2 || e.HLEnd != 2 || len(e.Lines) != 5 {
		t.Errorf("context should clamp at line 1: %+v", e)
	}
	b := ev["bug"].Excerpts
	if len(b) != 1 || b[0].Path != "model/tx.go" || b[0].Start != 47 || len(b[0].Lines) != 4 {
		t.Errorf("bug excerpt should clamp at EOF: %+v", b)
	}
}

func TestExcerptCaps(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "big.go"), numberedLines(500))
	nodes := []Node{{ID: "fn", Group: "up", Kind: "function", Code: []string{"big.go:10-400"}}}
	ev, _, _, err := attachEvidence(nodes, []SourceRoot{{Dir: root}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := ev["fn"].Excerpts[0]
	if len(e.Lines) != ExcerptMaxLines || !e.Truncated || e.Start != 7 {
		t.Errorf("per-ref cap: %d lines, truncated %v, start %d", len(e.Lines), e.Truncated, e.Start)
	}

	long := strings.Repeat("x", 4000) + "\n"
	writeFile(t, filepath.Join(root, "wide.go"), strings.Repeat(long, 200))
	var many []Node
	for i := 0; i < 20; i++ {
		many = append(many, Node{ID: fmt.Sprintf("n%d", i), Group: "up", Kind: "function", Code: []string{fmt.Sprintf("wide.go:%d-%d", i*5+4, i*5+8)}})
	}
	ev, _, warns, err := attachEvidence(many, []SourceRoot{{Dir: root}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, ne := range ev {
		for _, e := range ne.Excerpts {
			for _, l := range e.Lines {
				total += len(l) + 1
			}
		}
	}
	if total > ExcerptMaxBytes {
		t.Errorf("total excerpt bytes %d > cap %d", total, ExcerptMaxBytes)
	}
	if len(ev) == len(many) || len(ev) == 0 {
		t.Errorf("byte cap should keep some refs and drop others: kept %d of %d", len(ev), len(many))
	}
	capWarn := 0
	for _, w := range warns {
		if strings.Contains(w, "cap") {
			capWarn++
		}
	}
	if capWarn != 1 {
		t.Errorf("want exactly one cap warning, got %v", warns)
	}
}

func TestExcerptRejectsEscapesAndWarnsOnMissing(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	writeFile(t, filepath.Join(root, "ok.go"), numberedLines(5))
	writeFile(t, filepath.Join(dir, "secret.go"), numberedLines(5))
	if err := os.Symlink(filepath.Join(dir, "secret.go"), filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	nodes := []Node{{ID: "fn", Group: "up", Kind: "function", Code: []string{
		"../secret.go:1",
		"sub/../../secret.go:1",
		filepath.Join(dir, "secret.go") + ":1",
		"link.go:1",
		"gone.go:3",
		"ok.go:99",
		"ok.go:2",
	}}}
	ev, _, warns, err := attachEvidence(nodes, []SourceRoot{{Dir: root}}, nil)
	if err != nil {
		t.Fatalf("missing and escaping refs must warn, not fail: %v", err)
	}
	ex := ev["fn"].Excerpts
	if len(ex) != 1 || ex[0].Ref != "ok.go:2" {
		t.Fatalf("only ok.go:2 should be read, got %+v", ex)
	}
	if len(warns) != 6 {
		t.Fatalf("want 6 warnings, got %d: %v", len(warns), warns)
	}
	joined := strings.Join(warns, "\n")
	for _, want := range []string{"leaves the source root", "is absolute", "link.go not found", "gone.go not found", "only 5 lines"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
	for _, p := range []string{"..", "../x", "a/../../x", "/etc/passwd", `C:\x`, ""} {
		if _, err := cleanRelPath(p); err == nil {
			t.Errorf("cleanRelPath(%q) should fail", p)
		}
	}
	if c, err := cleanRelPath("./a//b/../c.go"); err != nil || c != "a/c.go" {
		t.Errorf("cleanRelPath = %q, %v", c, err)
	}
}

const testPatch = `diff --git a/model/tx.go b/model/tx.go
index 1111111..2222222 100644
--- a/model/tx.go
+++ b/model/tx.go
@@ -8,4 +8,5 @@ func split() {
 line 8
-line 9
+line 9 fixed
+line 9b
 line 10
 line 11
@@ -40 +41 @@
-line 40
+line 40 fixed
\ No newline at end of file
diff --git a/queue.go b/queue.go
--- a/queue.go
+++ b/queue.go
@@ -3,3 +3,4 @@
 line 3
+inserted

 line 4
diff --git a/new.go b/new.go
--- /dev/null
+++ b/new.go
@@ -0,0 +1,1 @@
+package x
`

func TestParsePatch(t *testing.T) {
	hs, err := ParsePatch("fix.patch", []byte(testPatch))
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 4 {
		t.Fatalf("hunks = %d, want 4: %+v", len(hs), hs)
	}
	h := hs[0]
	if h.Patch != "fix.patch" || h.File != "model/tx.go" || h.OldStart != 8 || h.OldLines != 4 || h.NewStart != 8 || h.NewLines != 5 || h.Section != "func split() {" || len(h.Lines) != 6 {
		t.Errorf("hunk 0 = %+v", h)
	}
	if h := hs[1]; h.OldStart != 40 || h.OldLines != 1 || h.NewLines != 1 || len(h.Lines) != 2 {
		t.Errorf("hunk 1 = %+v", h)
	}
	if h := hs[2]; h.File != "queue.go" || len(h.Lines) != 4 || h.Lines[2] != " " {
		t.Errorf("hunk 2 (blank context line) = %+v", h)
	}
	if h := hs[3]; h.File != "new.go" || h.OldLines != 0 {
		t.Errorf("hunk 3 (/dev/null) = %+v", h)
	}

	for name, bad := range map[string]string{
		"short":     "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n a\n",
		"header":    "--- a/x\n+++ b/x\n@@ -x +1 @@\n",
		"orphan":    "@@ -1 +1 @@\n a\n",
		"bad line":  "--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n a\n?b\n",
		"no header": "--- a/x\n+++ b/x\n@@ -1 +1\n",
	} {
		if _, err := ParsePatch("p", []byte(bad)); err == nil {
			t.Errorf("%s: ParsePatch should fail", name)
		}
	}
}

func TestPatchHunksMatchNodes(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "upstream")
	writeFile(t, filepath.Join(root, "model", "tx.go"), numberedLines(50))
	writeFile(t, filepath.Join(root, "queue.go"), numberedLines(10))
	patch := filepath.Join(dir, "fix.patch")
	writeFile(t, patch, testPatch)

	nodes := []Node{
		{ID: "a", Group: "up", Kind: "function", Code: []string{"model/tx.go:9"}},                // hunk 0
		{ID: "b", Group: "bugs", Kind: "bug", Code: []string{"upstream/model/tx.go:30-45"}},      // hunk 1, via root-name prefix
		{ID: "c", Group: "up", Kind: "function", Code: []string{"model/tx.go:12-20"}},            // touches nothing
		{ID: "d", Group: "up", Kind: "function", Code: []string{"queue.go:4", "model/tx.go:11"}}, // hunks 2 and 0
		{ID: "e", Group: "up", Kind: "function", Code: []string{"other/tx.go:9"}},                // same base name, other dir
	}
	ev, hunks, warns, err := attachEvidence(nodes, []SourceRoot{{Group: "up", Dir: root}}, []string{patch})
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 4 {
		t.Fatalf("hunks = %d, want 4", len(hunks))
	}
	want := map[string][]int{"a": {0}, "b": {1}, "d": {0, 2}}
	for id, w := range want {
		if ev[id] == nil || fmt.Sprint(ev[id].Hunks) != fmt.Sprint(w) {
			t.Errorf("node %s hunks = %v, want %v", id, ev[id], w)
		}
	}
	if ev["c"] != nil && len(ev["c"].Hunks) != 0 {
		t.Errorf("node c should match no hunk: %v", ev["c"].Hunks)
	}
	if ev["e"] != nil && len(ev["e"].Hunks) != 0 {
		t.Errorf("node e is a different file: %v", ev["e"].Hunks)
	}
	if d := ev["d"]; fmt.Sprint(d.Excerpts[0].Hunks) != "[2]" || fmt.Sprint(d.Excerpts[1].Hunks) != "[0]" {
		t.Errorf("per-excerpt hunks: %+v", d.Excerpts)
	}
	// Hunks match on ref paths even with no source root.
	ev2, _, _, err := attachEvidence(nodes[:1], nil, []string{patch})
	if err != nil {
		t.Fatal(err)
	}
	if ev2["a"] == nil || fmt.Sprint(ev2["a"].Hunks) != "[0]" || len(ev2["a"].Excerpts) != 0 {
		t.Errorf("patch-only evidence = %+v", ev2["a"])
	}
	unmatched := 0
	for _, w := range warns {
		if strings.Contains(w, "matched no node") {
			unmatched++
		}
	}
	if unmatched != 1 {
		t.Errorf("want 1 unmatched-hunk warning (new.go), got %v", warns)
	}
}

func TestParseSourceRoots(t *testing.T) {
	dir := t.TempDir()
	spec := evidenceSpec()
	roots, err := ParseSourceRoots([]string{"up=" + dir, dir}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if roots[0] != (SourceRoot{Group: "up", Dir: dir}) || roots[1] != (SourceRoot{Dir: dir}) {
		t.Errorf("roots = %+v", roots)
	}
	eqDir := filepath.Join(dir, "a=b")
	if err := os.Mkdir(eqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if r, err := ParseSourceRoots([]string{eqDir}, spec); err != nil || r[0].Group != "" {
		t.Errorf("a dir containing = is a plain root: %+v, %v", r, err)
	}
	for _, bad := range []string{"nope=" + dir, "up=" + filepath.Join(dir, "missing"), filepath.Join(dir, "missing")} {
		if _, err := ParseSourceRoots([]string{bad}, spec); err == nil {
			t.Errorf("ParseSourceRoots(%q) should fail", bad)
		}
	}
}

func TestSpecMoneyAndStepNames(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "map.json")
	writeFile(t, p, `{
  "kavach_map": 1, "title": "T",
  "groups": [{"id": "g", "title": "G"}],
  "nodes": [{"id": "n", "group": "g", "kind": "entry", "title": "N"}],
  "edges": [],
  "money": {"fields": ["amt", "src_bal"], "minor_units": 2, "currency": "usd"},
  "step_names": {"captures/01_split.tokenized.kavach": {"1": "Fund", "015": "Split"}}
}`)
	s, err := LoadSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Money == nil || fmt.Sprint(s.Money.Fields) != "[amt src_bal]" || s.Money.MinorUnits != 2 || s.Money.Currency != "USD" {
		t.Errorf("money = %+v", s.Money)
	}
	if got := s.StepNames["01_split"]; got["1"] != "Fund" || got["15"] != "Split" || len(s.StepNames) != 1 {
		t.Errorf("step_names = %v", s.StepNames)
	}
	m, err := Build(Input{Spec: s, BaseDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if m.Money == nil || m.Money.Currency != "USD" || m.StepNames["01_split"]["15"] != "Split" {
		t.Errorf("model money/step_names = %+v %v", m.Money, m.StepNames)
	}

	bad := &Spec{
		Version: 1, Groups: []Group{{ID: "g"}}, Nodes: []Node{{ID: "n", Group: "g", Kind: "entry"}},
		Money:     &Money{MinorUnits: 9},
		StepNames: map[string]map[string]string{"a": {"x": "X"}, "a.kavach": {"1": "Y"}, "": {"1": "Z"}},
	}
	err = bad.NormalizeAndValidate()
	if err == nil {
		t.Fatal("invalid money/step_names should fail")
	}
	for _, want := range []string{"fields must not be empty", "minor_units", "not a number", "more than once", "empty fixture key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestOldSpecModelUnchanged checks that a spec without the new fields, built
// without --source or --patch, yields a model with none of the new keys and
// the same JSON whether loaded from disk or passed in.
func TestOldSpecModelUnchanged(t *testing.T) {
	m := deckModel(t)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"money"`, `"step_names"`, `"evidence"`, `"hunks"`, `"excerpts"`} {
		if bytes.Contains(data, []byte(key)) {
			t.Errorf("old-spec model has new key %s", key)
		}
	}
	if len(m.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", m.Warnings)
	}

	dir := t.TempDir()
	specJSON := `{"kavach_map":1,"title":"Old","groups":[{"id":"g","title":"G","code_base":"https://example.com/blob/abc/"}],
"nodes":[{"id":"fn","group":"g","kind":"function","title":"F","code":["a.go:3-4"]}],"edges":[]}`
	p := filepath.Join(dir, "old.json")
	writeFile(t, p, specJSON)
	s, err := LoadSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(Input{Spec: s, BaseDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var raw Spec
	if err := json.Unmarshal([]byte(specJSON), &raw); err != nil {
		t.Fatal(err)
	}
	b, err := Build(Input{Spec: &raw, BaseDir: dir, Sources: nil, Patches: nil})
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Errorf("models differ:\n%s\n%s", ja, jb)
	}
	// Captured from origin/main before this change.
	want := `{"kavach_map_model":1,"generator":"kavach","title":"Old","groups":[{"id":"g","title":"G","code_base":"https://example.com/blob/abc/"}],"nodes":[{"id":"fn","group":"g","kind":"function","title":"F","code":["a.go:3-4"],"links":[{"text":"a.go:3-4","url":"https://example.com/blob/abc/a.go#L3-L4"}]}],"edges":null,"stats":{"fn":{}}}`
	if string(ja) != want {
		t.Errorf("old-spec model JSON changed:\n got %s\nwant %s", ja, want)
	}
}

// TestDeckMoneyAndStepNamesNode checks the deck's money formatting and step
// naming against a real Model. Skipped without node.
func TestDeckMoneyAndStepNamesNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	m := deckModel(t)
	m.Money = &Money{Fields: []string{"amt"}, MinorUnits: 2, Currency: "USD"}
	m.StepNames = map[string]map[string]string{"deck": {"3": "Pay"}}
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
const M = model.money;
const f = (v, mo) => KavachDeck.formatMoney(v, mo || M);
console.log(JSON.stringify({
  names: D.steps.map((s) => s.name),
  money: [f("2000"), f("2050"), f("-123456789"), f("5"), f("0"), f("-0"), f("12.5"), f("abc"),
    f("2000", { minor_units: 0, currency: "JPY" }), f("1999", { minor_units: 2, currency: "CHF" }), f("7", { minor_units: 2 })],
}));`
	out, err := exec.Command(node, "-e", script, deckPath, modelPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Names []string `json:"names"`
		Money []string `json:"money"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if fmt.Sprint(got.Names) != "[pay Pay]" {
		t.Errorf("step names = %v, want derived name for seq 1 and Pay for seq 3", got.Names)
	}
	want := []string{"$20", "$20.50", "-$1,234,567.89", "$0.05", "$0", "$0", "12.5", "abc", "¥2,000", "CHF 19.99", "0.07"}
	if fmt.Sprint(got.Money) != fmt.Sprint(want) {
		t.Errorf("money = %q\nwant %q", got.Money, want)
	}
}

package harbor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiffFromLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "grade.log")
	report := "[ok]: a test (1 ms)\n" +
		"fixture   /task/incident.kavach\n" +
		"old       invariant_violated(bit-range-syntax)@97\n" +
		"new       fixed\n" +
		"first divergence at step seq 97: output 0 of step 97 differs\n" +
		"  old  reply  :-1\n" +
		"variants  46 of 64 reproduce the incident on the old build · 46 of those pass on the new build\n" +
		"regressions 46 of 46 match the old build · 48 variants of 6 earlier inputs resembling the failing one\n" +
		"\x1b[1mverdict\x1b[0m   fixed\n" +
		"wall      206.2 ms for 206 replays\n"
	if err := os.WriteFile(log, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	d := diffFromLog(log)
	if d == nil {
		t.Fatal("no report read")
	}
	if d.Source != "log" || d.Verdict != "fixed" || d.Old != "invariant_violated(bit-range-syntax)@97" || d.New != "fixed" {
		t.Errorf("got %+v", d)
	}
	if d.Divergence == nil || d.Divergence.Seq != 97 || d.Divergence.Output != 0 {
		t.Errorf("divergence %+v", d.Divergence)
	}
	if d.VariantsLine == "" || d.RegressionsLine == "" {
		t.Errorf("summary lines missing: %+v", d)
	}
	if diffFromLog(filepath.Join(t.TempDir(), "none")) != nil {
		t.Error("a missing log gave a report")
	}
}

func TestReadDiffReport(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kavach-diff.json")
	body := `{"verdict":"variant_failed(7)@23","status":"variant_failed","old":"still_failing@32","new":"fixed",
"divergence":{"seq":32,"output":0,"detail":"output 0 of step 32 differs"},
"variants":{"candidates":56,"reproducing":47,"passing":39,"required":10,"checks":[
 {"id":7,"incident_seq":23,"mutation":"failing input (seq 32): \"type\" \"deposit\" → \"transfer\"","old":"still_failing@23","new":"still_failing@23","passed":false,"reproduces":true}]},
"regressions":{"candidates":48,"counted":46,"passing":46,"neighbours":4,"checks":[
 {"id":1,"input_seq":4,"mutation":"input (seq 5): \"id\" \"evt-002\" → \"evt-001\"","old":"ok","new":"ok","passed":true,"counted":true}]}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d := readDiffReport(p)
	if d == nil || d.Source != "json" || d.Verdict != "variant_failed(7)@23" {
		t.Fatalf("got %+v", d)
	}
	if d.Variants == nil || d.Variants.Reproducing != 47 || len(d.Variants.Checks) != 1 || d.Variants.Checks[0].Passed || *d.Variants.Checks[0].Reproduces != true {
		t.Errorf("variants %+v", d.Variants)
	}
	if d.Regressions == nil || d.Regressions.Neighbours != 4 || *d.Regressions.Checks[0].Counted != true {
		t.Errorf("regressions %+v", d.Regressions)
	}
	if d.Divergence == nil || d.Divergence.Seq != 32 {
		t.Errorf("divergence %+v", d.Divergence)
	}
}

func TestTrialIncident(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "job", "task__abc")
	v := filepath.Join(dir, "verifier")
	if err := os.MkdirAll(v, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "examples", "ledger", "testdata", "null-amount.kavach"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"config.json":              `{}`,
		"verifier/incident.kavach": string(fixture),
		"verifier/replay-new.json": `{"fixture":"/task/incident.kavach","result":{"status":"fixed","steps":[{"seq":1,"outputs":[{"sink":"s","data":"aGk="}]}]},"verdict":"fixed"}`,
		"verifier/replay-old.json": `not json`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reader{Dir: root}
	inc, err := r.TrialIncident("job", "task__abc")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Kind != "incident" || inc.Service != "ledger" || len(inc.Steps) == 0 {
		t.Fatalf("got kind %q service %q, %d steps", inc.Kind, inc.Service, len(inc.Steps))
	}
	if inc.Failure == nil || inc.Failure.Seq != 32 || inc.Failure.Kind != "panic" {
		t.Errorf("failure %+v", inc.Failure)
	}
	if inc.New == nil || inc.New.Verdict != "fixed" || len(inc.New.Steps) != 1 || inc.New.Steps[0].Outputs[0].Data != "hi" {
		t.Errorf("new %+v", inc.New)
	}
	if inc.Old != nil {
		t.Errorf("an unreadable old replay decoded: %+v", inc.Old)
	}
	if inc.Diff != nil {
		t.Errorf("no report, but diff %+v", inc.Diff)
	}
	if _, err := r.TrialIncident("job", "nope"); err != ErrNotFound {
		t.Errorf("missing trial: %v", err)
	}
}

func TestTrialIncidentTraffic(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "job", "task__abc")
	cdir := filepath.Join(dir, filepath.FromSlash(contractSubdir))
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "examples", "ledger", "testdata", "null-amount.kavach"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "traffic.kavach"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	inc, err := (&Reader{Dir: root}).TrialIncident("job", "task__abc")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Kind != "traffic" || len(inc.Steps) == 0 || inc.Diff != nil || inc.New != nil {
		t.Errorf("got kind %q, %d steps, diff %v, new %v", inc.Kind, len(inc.Steps), inc.Diff, inc.New)
	}
}

// The task's incident.kavach sits beside the Dockerfile that task.toml's
// verifier image is built from.
func TestTaskFixture(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "harbor", "tasks", "demo-kavach")
	writeFile(t, filepath.Join(task, "task.toml"), "[verifier.environment]\ndocker_image = \"ghcr.io/kavachlabs/bench-demo:grader-0123456789ab\"\n")
	writeFile(t, filepath.Join(task, "tests", "docker-compose.yaml"), "services:\n  main: {}\n")
	want := filepath.Join(root, "tasks", "demo", "incident.kavach")
	writeFile(t, want, "x")
	trial := t.TempDir()
	writeFile(t, filepath.Join(trial, "config.json"), `{"task":{"path":"`+task+`"}}`)
	if got := taskFixture(trial); got != want {
		t.Errorf("taskFixture = %q, want %q", got, want)
	}
}

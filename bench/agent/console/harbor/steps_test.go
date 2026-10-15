package harbor

import (
	"os"
	"strings"
	"testing"
)

const passTrial = "testdata/jobs/opencode-glm/redis-10068-kavach__Pass001"

func kinds(steps []Step) map[string]int {
	n := map[string]int{}
	for _, s := range steps {
		n[s.Kind]++
	}
	return n
}

func TestStepsFromTrajectory(t *testing.T) {
	steps, err := (&Reader{Dir: jobsDir}).Steps("opencode-glm", "redis-10068-kavach__Pass001")
	if err != nil {
		t.Fatal(err)
	}
	// 45 tool calls: 25 reads, 14 bash, 3 grep, 1 edit and 2 Kavach tools.
	want := map[string]int{KindText: 12, KindRead: 25, KindShell: 14, KindSearch: 3, KindEdit: 1, KindMCP: 2, KindUsage: 41}
	if got := kinds(steps); len(got) != len(want) {
		t.Fatalf("kinds %v, want %v", got, want)
	} else {
		for k, n := range want {
			if got[k] != n {
				t.Errorf("%s: %d steps, want %d", k, got[k], n)
			}
		}
	}
	if steps[0].TMs != 0 || steps[len(steps)-1].TMs <= 0 {
		t.Errorf("t_ms from %d to %d", steps[0].TMs, steps[len(steps)-1].TMs)
	}
	var diff *Step
	for i, s := range steps {
		switch {
		case s.Tool == "kavach_kavach_diff":
			diff = &steps[i]
		case s.Kind == KindEdit:
			if s.Title != "/work/src/t_stream.c" || !strings.Contains(s.Detail, "+            int64_t ms_delta = lpGetInteger(p);") {
				t.Errorf("edit %+v", s)
			}
		case s.Kind == KindShell && s.Exit != nil:
			t.Errorf("ATIF has no exit status: %+v", s)
		}
	}
	if diff == nil || !diff.Kavach || diff.Verdict != "fixed" || diff.Passed == nil || !*diff.Passed ||
		diff.Title != "kavach diff ./src/redis-server" || !strings.HasPrefix(diff.Variants, "49 of 64 variants") {
		t.Fatalf("kavach diff step %+v", diff)
	}
}

// OpenCode's log and the ATIF Harbor makes from it give the same attempt.
func TestStepsFromOpenCodeLog(t *testing.T) {
	f, err := os.Open(passTrial + "/agent/opencode.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fromLog, err := openCodeSteps(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(passTrial + "/agent/trajectory.json")
	if err != nil {
		t.Fatal(err)
	}
	fromATIF, err := atifSteps(b)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := kinds(fromLog), kinds(fromATIF); len(a) != len(b) {
		t.Fatalf("log %v, ATIF %v", a, b)
	} else {
		for k := range a {
			if a[k] != b[k] {
				t.Errorf("%s: log %d, ATIF %d", k, a[k], b[k])
			}
		}
	}
	var logTok, atifTok Tokens
	for _, s := range fromLog {
		if s.Tokens != nil {
			logTok.add(*s.Tokens)
		}
	}
	for _, s := range fromATIF {
		if s.Tokens != nil {
			atifTok.add(*s.Tokens)
		}
	}
	if logTok != atifTok {
		t.Errorf("tokens: log %+v, ATIF %+v", logTok, atifTok)
	}

	first := fromLog[0]
	if first.Kind != KindShell || first.TMs != 274 || first.Title != "ls /work && git -C /work log --oneline -5" ||
		first.Exit == nil || *first.Exit != 0 || !strings.HasPrefix(first.Detail, "00-RELEASENOTES") {
		t.Fatalf("first step %+v", first)
	}
	for _, s := range fromLog {
		if s.Kind == KindRead && s.Title == "/work/src/t_stream.c lines 1–100" {
			return
		}
	}
	t.Error("no read step with its line range")
}

func TestStepsWhileRunning(t *testing.T) {
	r := &Reader{Dir: jobsDir}
	steps, err := r.Steps("opencode-glm", "redis-10068-control__Run0001")
	if err != nil {
		t.Fatal(err)
	}
	if n := kinds(steps); n[KindUsage] != 8 || n[KindText] != 1 {
		t.Fatalf("kinds %v", n)
	}
	steps, err = r.Steps("opencode-glm", "redis-10068-control__Err0001")
	if err != nil {
		t.Fatal(err)
	}
	last := steps[len(steps)-1]
	if last.Kind != KindError || last.Title != "APIError: Provider returned error: 429 rate limited" {
		t.Fatalf("last step %+v", last)
	}
	// A trial without agent logs (nop) has no steps.
	if steps, err := r.Steps("nop-running", "hello-world-control__p3PHoHF"); err != nil || steps == nil || len(steps) != 0 {
		t.Fatalf("%v %v", steps, err)
	}
}

func TestOpenCodeLogNoise(t *testing.T) {
	log := "npm warn something\n" +
		`{"type":"tool_use","timestamp":1000,"part":{"tool":"bash","state":{"status":"error","input":{"command":"x"},"error":"boom"}}}` + "\n" +
		`{"type":"tool_use","timestamp":1500,"part":{"tool":"todowrite","state":{"status":"completed","input":{},"output":"ok"}}}` + "\n" +
		`{"type":"step_finish","timestamp":2000,"part":{"tokens":{"input":5`
	steps, err := openCodeSteps(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Kind != KindError || steps[0].Title != "bash: boom" ||
		steps[1].Kind != KindMCP || steps[1].Title != "todowrite" || steps[1].TMs != 500 {
		t.Fatalf("%+v", steps)
	}
}

func TestClip(t *testing.T) {
	s := strings.Repeat("é", maxText)
	c := clip(s)
	if len(c) > maxText+len("…") || !strings.HasSuffix(c, "…") || !strings.HasPrefix(s, strings.TrimSuffix(c, "…")) {
		t.Fatalf("clipped to %d bytes", len(c))
	}
}

func TestContractCheckSteps(t *testing.T) {
	const report = `{"dir":"contract","passed":false,"verdict":"variant_failed(31)@433","wall_ms":5120,
"report":{"verdict":"variant_failed(31)@433","passed":false,"detail":"variant 31 (every clock read shifted): invariant_violated(message-ttl)@433",
"lock":{"state":"approved"},"feature_steps":[244,247,433],"new":"ok",
"changed":[{"seq":244,"recorded":[],"replay":[]},{"seq":433,"recorded":[],"replay":[]}],"unreached":[247],
"invariants":["message-ttl"],"declared":["queue-max-length","message-ttl"],
"variant_candidates":3,"variants_passed":1,"variants_preexisting":1,
"variant_kinds":{"clock":{"candidates":2,"passed":0,"preexisting":1},"field":{"candidates":1,"passed":1,"preexisting":0}},
"variant_failures":[{"id":31,"kind":"clock","mutation":"every clock read shifted","feature_seq":433,"verdict":"invariant_violated(message-ttl)@433","seq":433,"detail":"late"}],
"round":6,"streak":2}}`
	check := func(name string, s Step) {
		c := s.Contract
		if c == nil {
			t.Fatalf("%s: no contract report", name)
		}
		if c.Round != 6 || c.Streak != 2 || c.Verdict != "variant_failed(31)@433" || c.Passed {
			t.Errorf("%s: round %d streak %d verdict %s passed %v", name, c.Round, c.Streak, c.Verdict, c.Passed)
		}
		if len(c.Changed) != 2 || c.Changed[1] != 433 || len(c.Unreached) != 1 || len(c.Declared) != 2 {
			t.Errorf("%s: changed %v unreached %v declared %v", name, c.Changed, c.Unreached, c.Declared)
		}
		if c.VariantKinds["clock"].Candidates != 2 || len(c.VariantFailures) != 1 || c.VariantFailures[0].Kind != "clock" || *c.VariantFailures[0].Seq != 433 {
			t.Errorf("%s: kinds %v failures %+v", name, c.VariantKinds, c.VariantFailures)
		}
		if s.Verdict != c.Verdict || s.Passed == nil || *s.Passed {
			t.Errorf("%s: step verdict %q passed %v", name, s.Verdict, s.Passed)
		}
	}
	check("mcp", toolStep("kavach_kavach_contract_check", map[string]any{"new": "/tmp/new"}, report, nil))
	check("shell", toolStep("bash", map[string]any{"command": "kavach contract check --new /tmp/new --json"}, "building…\n"+report, nil))

	if s := toolStep("kavach_kavach_replay", map[string]any{}, `{"verdict":"fixed","passed":true}`, nil); s.Contract != nil {
		t.Errorf("replay step has a contract report: %+v", s.Contract)
	}
	if s := toolStep("bash", map[string]any{"command": "kavach contract check --new x"}, "KAVACH CONTRACT CHECK\nverdict pass", nil); s.Contract != nil {
		t.Errorf("text output parsed as a report: %+v", s.Contract)
	}
}

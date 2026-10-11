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

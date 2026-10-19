package harbor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing/fstest"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// The console reads attempts as a Kavach service: each input is one attempt
// as Harbor left it, and the output is the trial the console shows for it.
// Fixtures of the reader's own bugs then replay like any service's incidents
// (cmd/trials records them).

// AttemptInput is an attempt's files, as one Kavach input. Each field holds a
// file Harbor writes, and Log the events of OpenCode's log that the reader
// is checked on: the agent's Kavach tool calls, and the last few events,
// which say how the run ended. Whole logs run to MBs.
type AttemptInput struct {
	Job    string            `json:"job"`
	ID     string            `json:"id"`
	Lock   json.RawMessage   `json:"lock,omitempty"`
	Result json.RawMessage   `json:"result,omitempty"`
	Kavach json.RawMessage   `json:"kavach,omitempty"`
	Log    []json.RawMessage `json:"log,omitempty"`
	// What a remote sandbox's attempt has while its agent runs: the live
	// copy of OpenCode's event log, and its session export, as text.
	Live   string `json:"live,omitempty"`
	Export string `json:"export,omitempty"`
}

// files lays the input out as the trial directory it came from. A field that
// is missing or null is a file Harbor did not write.
func (a AttemptInput) files() fstest.MapFS {
	fsys := fstest.MapFS{}
	put := func(name string, b []byte) {
		if len(b) > 0 && string(b) != "null" {
			fsys[name] = &fstest.MapFile{Data: b}
		}
	}
	put("lock.json", a.Lock)
	put("result.json", a.Result)
	put("verifier/kavach.json", a.Kavach)
	var log bytes.Buffer
	for _, ev := range a.Log {
		log.Write(ev)
		log.WriteByte('\n')
	}
	put("agent/opencode.txt", log.Bytes())
	put("agent/opencode-live.txt", []byte(a.Live))
	put("agent/opencode-session.json", []byte(a.Export))
	return fsys
}

// TrialReader folds attempts into the trials the console shows.
type TrialReader struct {
	seen []seenTrial
}

type seenTrial struct {
	trial Trial
	// What the invariants read from the input itself, not from the trial.
	exception, logEndsOnError bool
}

// StepsReader reads attempts' steps as the console's steps view and grade
// do. It is a service of its own so that its outputs and fixtures do not
// change TrialReader's.
type StepsReader struct {
	seen []seenSteps
}

type seenSteps struct {
	id        string
	steps     []Step
	agentDiff string
}

// Handle reads one attempt's steps and emits its Kavach steps on "steps".
func (r *StepsReader) Handle(env kavach.Env, in kavach.Input) error {
	var a AttemptInput
	if err := json.Unmarshal(in.Data, &a); err != nil {
		return fmt.Errorf("attempt: %w", err)
	}
	steps, err := stepsFrom(a.files())
	if err != nil {
		return err
	}
	ks := KavachSteps{Steps: []KavachStep{}, AgentDiff: agentDiff(steps)}
	for _, st := range steps {
		if st.Kavach {
			ks.Steps = append(ks.Steps, KavachStep{Title: clipTo(st.Title, 120), Verdict: st.Verdict})
		}
	}
	b, err := json.Marshal(ks)
	if err != nil {
		return err
	}
	env.Emit("steps", b)
	r.seen = append(r.seen, seenSteps{id: a.ID, steps: steps, agentDiff: ks.AgentDiff})
	return nil
}

// Invariants are the console's rules for Kavach steps.
func (r *StepsReader) Invariants() []kavach.Invariant {
	return []kavach.Invariant{
		{Name: "kavach-report-shown", Check: func() error {
			// Read from the output, not the command: whatever ran it, a
			// report of the CLI's replay or diff is a Kavach step.
			for _, s := range r.seen {
				for _, st := range s.steps {
					if st.Kind != KindShell {
						continue
					}
					if v := reportVerdict(st.Detail); v != "" && (!st.Kavach || st.Verdict != v) {
						return fmt.Errorf("%s: %q printed Kavach's verdict %s; the step shows kavach %v, verdict %q", s.id, clipTo(st.Title, 80), v, st.Kavach, st.Verdict)
					}
				}
			}
			return nil
		}},
		{Name: "agent-diff-is-last-diff", Check: func() error {
			for _, s := range r.seen {
				want := ""
				for _, st := range s.steps {
					if st.Kavach && st.Verdict != "" && (st.Kind == KindShell && strings.Contains(st.Title, "kavach diff") || strings.HasSuffix(st.Tool, "kavach_diff")) {
						want = st.Verdict
					}
				}
				if s.agentDiff != want {
					return fmt.Errorf("%s: the agent's last kavach diff said %q; the grade reports %q", s.id, want, s.agentDiff)
				}
			}
			return nil
		}},
	}
}

// KavachSteps is what the console makes of an attempt's Kavach steps: the
// steps it marks, and the verdict the grade reports as the agent's own.
type KavachSteps struct {
	Steps     []KavachStep `json:"steps"`
	AgentDiff string       `json:"agent_diff,omitempty"`
}

// KavachStep is one step marked as Kavach's.
type KavachStep struct {
	Title   string `json:"title"`
	Verdict string `json:"verdict,omitempty"`
}

// Handle reads one attempt and emits its trial on the "trial" sink.
func (r *TrialReader) Handle(env kavach.Env, in kavach.Input) error {
	var a AttemptInput
	if err := json.Unmarshal(in.Data, &a); err != nil {
		return fmt.Errorf("attempt: %w", err)
	}
	t := trialFrom(a.Job, filepath.Join(a.Job, a.ID), a.files())
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	env.Emit("trial", b)

	s := seenTrial{trial: t}
	var res struct {
		Exception json.RawMessage `json:"exception_info"`
	}
	if json.Unmarshal(a.Result, &res) == nil {
		s.exception = len(res.Exception) > 0 && string(res.Exception) != "null"
	}
	if n := len(a.Log); n > 0 {
		var ev struct {
			Type string `json:"type"`
		}
		s.logEndsOnError = json.Unmarshal(a.Log[n-1], &ev) == nil && ev.Type == "error"
	}
	r.seen = append(r.seen, s)
	return nil
}

// Invariants are the console's rules for an attempt's status.
func (r *TrialReader) Invariants() []kavach.Invariant {
	return []kavach.Invariant{
		{Name: "provider-error-is-error", Check: func() error {
			// The model's provider ended the run, not the agent: it says
			// nothing about the agent, so it is not a failed attempt.
			for _, s := range r.seen {
				if s.exception && s.logEndsOnError && s.trial.Status == Fail {
					return fmt.Errorf("%s: OpenCode's log ends on an error, and the attempt counts as fail", s.trial.ID)
				}
			}
			return nil
		}},
		{Name: "pass-is-full-reward", Check: func() error {
			for _, s := range r.seen {
				t := s.trial
				if full := t.Reward != nil && *t.Reward >= 1; full != (t.Status == Pass) {
					return fmt.Errorf("%s: status %s with reward %v", t.ID, t.Status, rewardText(t.Reward))
				}
			}
			return nil
		}},
		{Name: "fail-is-graded", Check: func() error {
			for _, s := range r.seen {
				if s.trial.Status == Fail && s.trial.Reward == nil {
					return fmt.Errorf("%s: counts as fail without a reward", s.trial.ID)
				}
			}
			return nil
		}},
	}
}

// reportLine is a line of the kavach CLI's report: its fixture, or its
// verdict (replay prints it as result).
var reportLine = regexp.MustCompile(`(?m)^(fixture|verdict|result)\s+(\S+)\s*$`)

// reportVerdict is the verdict in the report of the CLI's replay or diff
// that out holds, or "" when out holds none.
func reportVerdict(out string) string {
	fixture, v := false, ""
	for _, m := range reportLine.FindAllStringSubmatch(ansiEsc.ReplaceAllString(out, ""), -1) {
		switch {
		case m[1] == "fixture":
			fixture = strings.HasSuffix(m[2], ".kavach")
		case fixture:
			v = m[2]
		}
	}
	return v
}

func rewardText(r *float64) string {
	if r == nil {
		return "none"
	}
	return fmt.Sprint(*r)
}

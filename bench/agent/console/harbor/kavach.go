package harbor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing/fstest"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// The console reads attempts as a Kavach service: each input is one attempt
// as Harbor left it, and the output is the trial the console shows for it.
// Fixtures of the reader's own bugs then replay like any service's incidents
// (cmd/trials records them).

// AttemptInput is an attempt's files, as one Kavach input. Each field holds a
// file Harbor writes, and Log the last events of OpenCode's log: the reader
// needs no more of it to classify the attempt, and whole logs run to MBs.
type AttemptInput struct {
	Job    string            `json:"job"`
	ID     string            `json:"id"`
	Lock   json.RawMessage   `json:"lock,omitempty"`
	Result json.RawMessage   `json:"result,omitempty"`
	Kavach json.RawMessage   `json:"kavach,omitempty"`
	Log    []json.RawMessage `json:"log,omitempty"`
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

func rewardText(r *float64) string {
	if r == nil {
		return "none"
	}
	return fmt.Sprint(*r)
}

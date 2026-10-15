package harbor

import (
	"testing"

	kavach "github.com/kavachlabs/kavach/sdk/go"
	"github.com/kavachlabs/kavach/sdk/go/kavachtest"
)

// Fixtures of the reader's own bugs, recorded by cmd/trials from real jobs
// with the bug in place and the neutral environment of
// testdata/kavach/facts.json (kavach-recorder --test-facts).
//
// provider-error.kavach: job 2026-10-15__15-47-13, read without the
// providerError case in trialFrom. Its eighth attempt to finish ended on
// OpenRouter's 429 and counted as fail.
func TestKavachFixtures(t *testing.T) {
	kavachtest.Run(t, "testdata/kavach/*.kavach", func() kavach.Handler { return &TrialReader{} })
}

// Fixtures of the steps reader, recorded the same way with
// cmd/trials -steps, from the same job.
//
// steps/cli-steps.kavach: read with only kavach contract check marked
// among shell steps. Attempt t88x7wA ran kavach replay and diff in the
// shell, and its steps were not marked as Kavach's.
//
// steps/agent-diff.kavach: read with agentDiff returning "". The grade
// reported no verdict of the agent's own for t88x7wA, which ran kavach diff.
func TestKavachStepsFixtures(t *testing.T) {
	kavachtest.Run(t, "testdata/kavach/steps/*.kavach", func() kavach.Handler { return &StepsReader{} })
}

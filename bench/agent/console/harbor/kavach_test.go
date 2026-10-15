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

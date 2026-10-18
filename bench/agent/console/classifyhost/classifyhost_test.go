package classifyhost_test

import (
	"testing"

	"github.com/kavachlabs/kavach/bench/agent/console/classifyhost"
	kavach "github.com/kavachlabs/kavach/sdk/go"
	"github.com/kavachlabs/kavach/sdk/go/kavachtest"
)

// The incident of public trial job patient-lemon-ibex-but7lma: a sandbox that
// failed to start was reported as a rejected key.
func TestIncidentFixtures(t *testing.T) {
	kavachtest.Run(t, "../testdata/*.kavach", func() kavach.Handler { return &classifyhost.Classifier{} })
}

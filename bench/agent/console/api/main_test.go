package api

import (
	"os"
	"testing"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// TestMain lets the test binary serve as bench's hidden helper, which runs
// the Harbor of a visitor's run.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == harbor.LogPipeCommand {
		os.Exit(harbor.RunLogPipe(os.Args[2:]))
	}
	os.Exit(m.Run())
}

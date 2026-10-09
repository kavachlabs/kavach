// Package testrecorder builds kavach-recorder for tests that record through
// the Go SDK.
package testrecorder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Run runs a package's tests with kavach-recorder built and $KAVACH_RECORDER
// pointing at it. Use it as
//
//	func TestMain(m *testing.M) { os.Exit(testrecorder.Run(m)) }
func Run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "kavach-recorder-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "kavach-recorder")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/kavachlabs/kavach/cmd/kavach-recorder").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build kavach-recorder: %v\n%s", err, out)
		return 1
	}
	os.Setenv("KAVACH_RECORDER", bin)
	return m.Run()
}

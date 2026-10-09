// Package kavachtest runs Kavach fixtures as ordinary Go tests.
package kavachtest

import (
	"path/filepath"
	"testing"

	"github.com/kavachlabs/kavach/replay"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// Run replays every fixture matching pattern (e.g. "testdata/*.kavach") as a
// subtest. A fixture passes when replay reports ok or fixed.
func Run(t *testing.T, pattern string, newHandler func() kavach.Handler) {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("kavachtest: no fixtures match %q", pattern)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			res, err := replay.RunFile(path, newHandler)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Passed() {
				t.Errorf("%s: %s", res, res.Detail)
			}
		})
	}
}

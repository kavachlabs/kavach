package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Fixtures of the console's own UI bugs, recorded by kavach/main.mjs from
// the sessions in kavach/sessions with the bug in place and the neutral
// environment of testdata/kavach/facts.json (kavach-recorder --test-facts).
//
// panes.kavach: the panes saved as only their widths. The person closed
// both panes, and a reload drew both open again.
//
// reload-flicker.kavach: the same session with the panes saved, and
// index.html not applying a closed left sidebar before the first paint. It
// showed and then slid shut.
//
// picker.kavach: every status poll redrawing the new-run form. A poll every
// 15s rebuilt the open model picker.
//
// They replay on the console's UI session (kavach/session.mjs) under Node,
// with the kavach CLI built from this repository. Without Node or the
// TypeScript SDK's build (sdk/typescript: npm ci && npm run build), the test
// is skipped.
func TestKavachFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sdk/typescript/dist/index.js")); err != nil {
		t.Skip("the TypeScript SDK is not built: cd sdk/typescript && npm ci && npm run build")
	}
	fixtures, err := filepath.Glob("testdata/kavach/*.kavach")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	cli := filepath.Join(t.TempDir(), "kavach")
	build := exec.Command("go", "build", "-o", cli, "./cmd/kavach")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the kavach CLI: %v\n%s", err, out)
	}
	host, err := filepath.Abs("kavach/main.mjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		t.Run(filepath.Base(f), func(t *testing.T) {
			out, err := exec.Command(cli, "replay", f, "--bin", node+" "+host).CombinedOutput()
			if err != nil {
				t.Fatalf("kavach replay: %v\n%s", err, out)
			}
		})
	}
}

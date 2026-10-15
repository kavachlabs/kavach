package harbor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const shiftPatch = `diff --git a/other.c b/other.c
--- a/other.c
+++ b/other.c
@@ -1,1 +1,5 @@
+x
diff --git a/f.c b/f.c
--- a/f.c
+++ b/f.c
@@ -3,3 +3,4 @@ fn
 three
+added
+added too
 four
-five
@@ -20,0 +21,2 @@
+after twenty
+and more
@@ -30,2 +33,1 @@
 thirty
-thirty-one
`

func TestShiftLine(t *testing.T) {
	for _, c := range []struct{ n, want int }{
		{1, 1}, {2, 2},
		{3, 3},   // context before the first addition
		{4, 6},   // pushed down by the two added lines
		{5, 0},   // removed
		{6, 7},   // after the hunk: +2 -1
		{20, 21}, // before the insertion that follows line 20
		{21, 24}, // after it
		{30, 33},
		{31, 0},
		{40, 42},
	} {
		if got := shiftLine([]byte(shiftPatch), "f.c", c.n); got != c.want {
			t.Errorf("line %d -> %d, want %d", c.n, got, c.want)
		}
	}
	if got := shiftLine([]byte(shiftPatch), "untouched.c", 7); got != 7 {
		t.Errorf("untouched file: %d", got)
	}
	if got := shiftLine(nil, "f.c", 7); got != 7 {
		t.Errorf("no patch: %d", got)
	}
}

func TestSnippetFiles(t *testing.T) {
	inv := &Invariants{Invariants: []InvariantDef{
		{Name: "a", DocAt: &Snippet{Path: "src/b.c"}, Code: []Snippet{{Path: "src/a.c"}, {Path: "src/b.c"}, {Path: "../escape"}, {Path: "/abs"}}},
		{Name: "b", Code: []Snippet{{Path: "src/a.c"}}},
	}}
	if got := snippetFiles(inv); !slices.Equal(got, []string{"src/a.c", "src/b.c"}) {
		t.Errorf("files %q", got)
	}
}

func TestMapSnippets(t *testing.T) {
	root := t.TempDir()
	// The agent's patch added two lines above the integration's code.
	agent := []byte("diff --git a/src/k.c b/src/k.c\n--- a/src/k.c\n+++ b/src/k.c\n@@ -1,1 +1,3 @@\n+// new\n+// new\n int a;\n")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	final := "// new\n// new\nint a;\nstatic int x;\n{\"inv-one\", f},\n"
	os.WriteFile(filepath.Join(root, "src", "k.c"), []byte(final), 0o644)
	srcs := []patchSource{{path: "/p/int.patch"}, {path: "/p/patch.diff", text: agent, agent: true}}
	inv := &Invariants{Invariants: []InvariantDef{{Name: "inv-one", Code: []Snippet{
		{Path: "src/k.c", Line: 2, Text: "static int x;\n{\"inv-one\", f},", File: "/p/int.patch"}, // lines 2..3 before the shift
		{Path: "src/k.c", Line: 3, Text: "not what is there", File: "/p/patch.diff"},
	}}}}
	mapSnippets(inv, root, srcs)
	c := inv.Invariants[0].Code
	if c[0].SrcLine != 4 || c[0].SrcHit != 5 || c[0].SrcFile != filepath.Join(root, "src", "k.c") {
		t.Errorf("shifted snippet %+v", c[0])
	}
	if c[1].SrcFile != "" {
		t.Errorf("a snippet that isn't in the file was placed: %+v", c[1])
	}
}

package harbor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pyPatch = `diff --git a/driver.py b/driver.py
new file mode 100644
--- /dev/null
+++ b/driver.py
@@ -0,0 +1,22 @@
+class Contract:
+    """What the store may send.
+
+    Invariants:
+      cookie-scope   every cookie sent is one the request
+                     allows for that URL;
+      cookie-limits  the store never holds more than max_cookies.
+    """
+
+queue-ttl
+    A message is never delivered after its time to live.
+
+    def invariants(self):
+        return [
+            Invariant("cookie-scope", lambda: self._check("cookie-scope")),
+            Invariant("cookie-limits", self._limits),
+        ]
+
+    def _limits(self):
+        if self.n > self.max:
+            raise AssertionError("too many")
+
`

const cPatch = `diff --git a/src/kavach.c b/src/kavach.c
--- a/src/kavach.c
+++ b/src/kavach.c
@@ -10,3 +10,11 @@ static int x;
 static int y;
+static int trimInvariant(void *state, void *arg, char **detail) {
+    if (ok) return KAVACH_OK;
+    return KAVACH_ERROR;
+}
+
+static const kavach_invariant invariants[] = {
+    {"stream-trim-contract", trimInvariant, NULL},
+};
`

const goAgentPatch = `diff --git a/kavach.go b/kavach.go
--- a/kavach.go
+++ b/kavach.go
@@ -1,6 +1,7 @@
 const (
 	invArgs = "streamed-call-args"
 )
+	// streamed-call-args: each streamed args object stays part of Args.
 	a.fail(invArgs, "call %s lost a field", id)
`

func TestFindInvariants(t *testing.T) {
	files := append(parsePatch([]byte(pyPatch), "/t/py.patch", false), parsePatch([]byte(cPatch), "/t/c.patch", false)...)
	files = append(files, parsePatch([]byte(goAgentPatch), "/t/patch.diff", true)...)
	defs := findInvariants(files, []string{"queue-ttl", "not-in-source"})
	got := map[string]InvariantDef{}
	var order []string
	for _, d := range defs {
		got[d.Name] = d
		order = append(order, d.Name)
	}
	if want := "queue-ttl cookie-scope cookie-limits stream-trim-contract streamed-call-args"; strings.Join(order, " ") != want {
		t.Fatalf("names %q, want %q", strings.Join(order, " "), want)
	}
	for name, doc := range map[string]string{
		"cookie-scope":         "every cookie sent is one the request allows for that URL;",
		"cookie-limits":        "the store never holds more than max_cookies.",
		"queue-ttl":            "A message is never delivered after its time to live.",
		"streamed-call-args":   "each streamed args object stays part of Args.",
		"stream-trim-contract": "",
	} {
		if got[name].Doc != doc {
			t.Errorf("%s: doc %q, want %q", name, got[name].Doc, doc)
		}
	}
	roles := func(d InvariantDef) string {
		var r []string
		for _, c := range d.Code {
			r = append(r, c.Role)
		}
		return strings.Join(r, " ")
	}
	if r := roles(got["cookie-limits"]); r != "declaration check" {
		t.Errorf("cookie-limits code %q", r)
	}
	if c := got["cookie-limits"].Code[1]; c.Line != 19 || !strings.Contains(c.Text, "too many") || strings.Contains(c.Text, "\n\n") {
		t.Errorf("cookie-limits check at %d: %q", c.Line, c.Text)
	}
	if c := got["stream-trim-contract"].Code; len(c) != 2 || c[1].Line != 11 || !strings.HasSuffix(c[1].Text, "}") {
		t.Errorf("stream-trim-contract code %+v", c)
	}
	if d := got["streamed-call-args"]; !d.Agent || roles(d) != "declaration fails" || !d.Code[1].Agent || d.Code[0].Agent {
		t.Errorf("streamed-call-args %+v", d)
	}
	if got["cookie-scope"].Agent {
		t.Error("cookie-scope marked as the agent's")
	}
}

// A snippet says which lines of its patch show it, removed lines and all.
func TestPatchLines(t *testing.T) {
	files := append(parsePatch([]byte(cPatch), "/t/c.patch", false), parsePatch([]byte(goAgentPatch+`@@ -20,4 +21,4 @@ func check() {
 	if bad {
-		a.fail("old", "x")
+		a.fail("streamed-call-args",
+			"call %s lost a field", id)
 	}
`), "/t/patch.diff", true)...)
	defs := findInvariants(files, nil)
	got := map[string]InvariantDef{}
	for _, d := range defs {
		got[d.Name] = d
	}
	type at struct {
		role, file    string
		line, fl, hit int
	}
	var have []at
	for _, name := range []string{"stream-trim-contract", "streamed-call-args"} {
		for _, c := range got[name].Code {
			have = append(have, at{c.Role, c.File, c.Line, c.FileLine, c.FileHit})
		}
	}
	want := []at{
		{"declaration", "/t/c.patch", 16, 11, 12},
		{"check", "/t/c.patch", 11, 6, 6},
		{"declaration", "/t/patch.diff", 1, 5, 6},
		{"fails", "/t/patch.diff", 4, 8, 8},
		{"fails", "/t/patch.diff", 21, 11, 13},
	}
	if len(have) != len(want) {
		t.Fatalf("snippets %+v", have)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("snippet %d: %+v, want %+v", i, have[i], want[i])
		}
	}
	if d := got["streamed-call-args"].DocAt; d == nil || d.File != "/t/patch.diff" || d.FileLine != 8 {
		t.Errorf("doc at %+v", d)
	}
}

// A task whose image copies in a whole Kavach driver, with no integration
// patch, has its invariants read from the driver.
func TestIntegrationDriver(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "harbor", "tasks", "demo-kavach")
	writeFile(t, filepath.Join(task, "task.toml"), "[environment]\nworkdir = \"/work\"\n")
	writeFile(t, filepath.Join(task, "environment", "docker-compose.yaml"), `services:
  main:
    build:
      context: ../../../..
      dockerfile: tasks/demo/Dockerfile
`)
	writeFile(t, filepath.Join(root, "tasks", "demo", "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(root, "tasks", "demo", "traffic.kavach"), "")
	driver := filepath.Join(root, "tasks", "demo", "kavach_demo.py")
	writeFile(t, driver, `class Handler:
    def invariants(self):
        return [check(n) for n in (
            # A message is never delivered
            # after its time to live.
            "queue-ttl",
            # Untouched, nothing to say.
            "queue-order",
        )]

    def _deliver(self):
        self._violate("queue-ttl", "expired")
`)

	got := integrationPatch(filepath.Join(root, "harbor", "tasks"), "demo-kavach")
	if got != driver {
		t.Fatalf("integration %q, want %q", got, driver)
	}
	b, _ := os.ReadFile(driver)
	defs := findInvariants(parsePatch(newFilePatch("kavach_demo.py", b), driver, false), nil)
	if len(defs) != 2 || defs[0].Name != "queue-ttl" || defs[1].Name != "queue-order" ||
		defs[0].Doc != "A message is never delivered after its time to live." || defs[0].DocAt == nil || defs[0].DocAt.Line != 4 {
		t.Fatalf("defs %+v", defs)
	}
	c := defs[0].Code
	if len(c) != 2 || c[0].Role != "declaration" || c[0].Path != "kavach_demo.py" || c[0].Line != 5 || c[1].Role != "fails" || c[1].Line != 11 {
		t.Errorf("code %+v", c)
	}
}

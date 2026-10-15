package harbor

import (
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
	files := append(parsePatch([]byte(pyPatch), false), parsePatch([]byte(cPatch), false)...)
	files = append(files, parsePatch([]byte(goAgentPatch), true)...)
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

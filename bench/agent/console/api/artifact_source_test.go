package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// A trial whose collect hook copied the source out needs no Docker.
func TestInvariantArtifactSource(t *testing.T) {
	tasks := filepath.Join(t.TempDir(), "harbor", "tasks")
	task := filepath.Join(tasks, "redis-10068-kavach")
	integration := "diff --git a/src/kavach.c b/src/kavach.c\n--- /dev/null\n+++ b/src/kavach.c\n@@ -0,0 +1,3 @@\n+static const kavach_invariant invariants[] = {\n+    {\"stream-trim-contract\", trimInvariant, NULL},\n+};\n"
	for name, text := range map[string]string{
		"task.toml":                                     "[environment]\ndocker_image = \"ghcr.io/kavachlabs/bench-redis-10068:kavach-0123456789ab\"\n",
		"../../../tasks/redis-10068/Dockerfile":         "FROM scratch\n",
		"../../../tasks/redis-10068/redis-kavach.patch": integration,
	} {
		p := filepath.Join(task, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jobs := copyJobs(t)
	trial := "/api/trials/redis-oracle/redis-10068-kavach__Dw5kmkd"
	file := "// agent\nstatic const kavach_invariant invariants[] = {\n    {\"stream-trim-contract\", trimInvariant, NULL},\n};\n"
	p := filepath.Join(jobs, "redis-oracle", "redis-10068-kavach__Dw5kmkd", "artifacts", "logs", "artifacts", "source", "src", "kavach.c")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ts := newServer(t, Config{JobsDir: jobs, TasksDir: tasks})

	var inv harbor.Invariants
	if code := get(t, ts.URL+trial+"/invariants", &inv); code != 200 {
		t.Fatalf("%d", code)
	}
	c := inv.Invariants[0].Code[0]
	if inv.Source != "artifacts" || inv.SourceNote != "" || !slices.Equal(inv.Files, []string{"src/kavach.c"}) || c.SrcFile != p || c.SrcLine != 2 || c.SrcHit != 3 {
		t.Errorf("source %q note %q files %q snippet %+v", inv.Source, inv.SourceNote, inv.Files, c)
	}
	resp, err := http.Get(ts.URL + trial + "/invariants/file?path=src/kavach.c")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != file {
		t.Errorf("file: %d %q", resp.StatusCode, b)
	}

	// A symlink, as the file or as its directory, is never followed.
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("secret"), 0o644)
	dir := filepath.Dir(p)
	os.Remove(p)
	os.Symlink(secret, p)
	var linked harbor.Invariants
	get(t, ts.URL+trial+"/invariants", &linked)
	if linked.Source != "patch" || linked.Invariants[0].Code[0].SrcFile != "" {
		t.Errorf("symlinked file: %q %+v", linked.Source, linked.Invariants[0].Code[0])
	}
	os.Remove(p)
	os.WriteFile(filepath.Join(dir, "kavach.c"), []byte(file), 0o644)
	os.Rename(dir, dir+"-real")
	os.Symlink(dir+"-real", dir)
	var dirLinked harbor.Invariants
	get(t, ts.URL+trial+"/invariants", &dirLinked)
	if dirLinked.Source != "patch" {
		t.Errorf("symlinked directory: %q", dirLinked.Source)
	}
}

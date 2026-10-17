package harbor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSyncPausedJobIsLeftAlone(t *testing.T) {
	s, fake := syncFixture(t)
	writeFile(t, filepath.Join(s.Runner.JobsDir, "p", "config.json"), "{}")
	writeFile(t, s.Runner.file("p", ".json"), "{}")
	writeFile(t, s.Runner.file("p", ".paused"), "")
	if j, _ := s.Reader.Job("p"); j.Status != JobPaused {
		t.Fatalf("status %s", j.Status)
	}
	if _, err := s.RunOnce(context.Background(), "p"); !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("err %v", err)
	}
	s.Sweep()
	time.Sleep(100 * time.Millisecond)
	if fake.count() != 0 {
		t.Error("uploaded a paused job")
	}
}

// A new Harbor for a job stops the sync in progress, which leaves the job
// pending, and the Harbor's exit syncs it again.
func TestSyncStopsWhenHarborStarts(t *testing.T) {
	runner, _ := fakeHarbor(t, `
if [ -e "$JOBS/ran" ]; then
  trap 'exit 130' INT
  while :; do sleep 0.05; done
fi
touch "$JOBS/ran"
mkdir -p "$JOBS/demo" && echo '{}' > "$JOBS/demo/config.json"
echo one > "$JOBS/demo/a.txt"; echo two > "$JOBS/demo/b.txt"
`)
	t.Setenv("JOBS", runner.JobsDir)
	t.Setenv("R2_BUCKET", "")
	writeFile(t, runner.EnvFile, "R2_ACCOUNT_ID=acct\nR2_ACCESS_KEY_ID="+testAccessKey+"\nR2_SECRET_ACCESS_KEY="+testSecretKey+"\n")
	fake, ts := newFakeS3(t)
	s := &Syncer{Runner: runner, Endpoint: ts.URL, Backoff: time.Millisecond}
	s.Reader = &Reader{Dir: runner.JobsDir, Runs: runner, Sync: s}
	runner.OnStart, runner.OnExit = s.Cancel, s.Queue
	t.Cleanup(func() { runner.Stop("demo") })

	if _, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	runner.Wait("demo")
	waitFor(t, func() bool { return s.State("demo").State == SyncSynced })
	before := fake.count()
	// Changed files, so there is something to upload when the Harbor starts.
	later := time.Now().Add(time.Minute)
	for _, f := range []string{"a.txt", "b.txt"} {
		p := filepath.Join(runner.JobsDir, "demo", f)
		writeFile(t, p, strings.ToUpper(f))
		os.Chtimes(p, later, later)
	}

	s.afterScan = func(string) {
		s.afterScan = nil
		if err := runner.Resume("demo"); err != nil {
			t.Error(err)
		}
	}
	if _, err := s.RunOnce(context.Background(), "demo"); !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("err %v", err)
	}
	if st := s.State("demo"); st.State != SyncPending || st.Error != "" {
		t.Errorf("after the cut: %+v", st)
	}
	if fake.count() != before {
		t.Errorf("%d files went up after the Harbor started", fake.count()-before)
	}

	runner.Stop("demo")
	runner.Wait("demo")
	waitFor(t, func() bool { return s.State("demo").State == SyncSynced })
	if got, _ := fake.get("harbor/demo/b.txt"); string(got) != "B.TXT" {
		t.Errorf("b.txt %q", got)
	}
}

// What is uploaded is what was scanned: a file rewritten between the two
// does not go up, even with the same size.
func TestSyncFileChangedAfterScan(t *testing.T) {
	s, fake := syncFixture(t)
	dir := finishedJob(t, s, "j1")
	victim := filepath.Join(dir, "victim.txt")
	writeFile(t, victim, strings.Repeat("a", len(testORKey)))
	s.afterScan = func(path string) {
		if path == victim {
			writeFile(t, victim, testORKey)
		}
	}
	_, err := s.RunOnce(context.Background(), "j1")
	if err == nil || !strings.Contains(err.Error(), "victim.txt changed while uploading") {
		t.Fatalf("err %v", err)
	}
	fake.mu.Lock()
	for path, body := range fake.objects {
		if strings.Contains(path, "victim") || strings.Contains(string(body), "sk-or-v1-") {
			t.Errorf("uploaded %s", path)
		}
	}
	fake.mu.Unlock()
	if st := s.State("j1"); st.State != SyncFailed {
		t.Errorf("%+v", st)
	}

	// The next sync scans the new content and holds it back.
	s.afterScan = nil
	res, err := s.RunOnce(context.Background(), "j1")
	if err != nil || res.Skipped != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, ok := fake.get("harbor/j1/victim.txt"); ok {
		t.Error("victim.txt uploaded")
	}
}

func TestSyncErrorsHideTheEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	writeFile(t, path, "x")
	sum := sha256.Sum256([]byte("x"))
	want := hex.EncodeToString(sum[:])

	closed := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()
	c := &S3{Endpoint: "http://" + host, Bucket: "b", AccessKey: "AK", SecretKey: "SK", Backoff: time.Millisecond}
	err := c.Put(context.Background(), "harbor/j/f", path, 1, "text/plain", want)
	if err == nil || strings.Contains(err.Error(), host) || strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(err.Error(), "PUT harbor/j/f") {
		t.Errorf("dial error %v", err)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message><AWSAccessKeyId>AKIDLEAK</AWSAccessKeyId><HostId>acct123</HostId></Error>`)
	}))
	t.Cleanup(denied.Close)
	c.Endpoint = denied.URL
	err = c.Put(context.Background(), "harbor/j/f", path, 1, "text/plain", want)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "AccessDenied Access Denied") ||
		strings.Contains(err.Error(), "AKIDLEAK") || strings.Contains(err.Error(), "acct123") {
		t.Errorf("403 error %v", err)
	}

	dns := &url.Error{Op: "Put", URL: "https://acct123.r2.cloudflarestorage.com/b/k", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "acct123.r2.cloudflarestorage.com"}}}
	if msg := shortErr(dns, "acct123.r2.cloudflarestorage.com"); msg != "lookup: no such host" {
		t.Errorf("dns error %q", msg)
	}
}

// A sweep that finds nothing to do leaves the manifest and the time alone.
func TestSyncNoopKeepsManifestAndTime(t *testing.T) {
	s, _ := syncFixture(t)
	finishedJob(t, s, "j1")
	if _, err := s.RunOnce(context.Background(), "j1"); err != nil {
		t.Fatal(err)
	}
	st1 := s.State("j1")
	info1, _ := os.Stat(s.manifestPath("j1"))
	time.Sleep(20 * time.Millisecond)
	if res, err := s.RunOnce(context.Background(), "j1"); err != nil || res.Uploaded != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	st2 := s.State("j1")
	info2, _ := os.Stat(s.manifestPath("j1"))
	if !st2.At.Equal(*st1.At) || !info2.ModTime().Equal(info1.ModTime()) {
		t.Errorf("rewritten: %v -> %v, manifest %v -> %v", st1.At, st2.At, info1.ModTime(), info2.ModTime())
	}
}

// A sync by another process (bench sync) shows without a restart.
func TestSyncStateFollowsTheManifest(t *testing.T) {
	s, _ := syncFixture(t)
	dir := finishedJob(t, s, "j1")
	if _, err := s.RunOnce(context.Background(), "j1"); err != nil {
		t.Fatal(err)
	}
	first := s.State("j1")
	other := &Syncer{Runner: s.Runner, Reader: s.Reader, Endpoint: s.Endpoint, Backoff: time.Millisecond}
	time.Sleep(20 * time.Millisecond)
	writeFile(t, filepath.Join(dir, "new.txt"), "n")
	if _, err := other.RunOnce(context.Background(), "j1"); err != nil {
		t.Fatal(err)
	}
	if got := s.State("j1"); got.Files != first.Files+1 || !got.At.After(*first.At) {
		t.Errorf("%+v then %+v", first, got)
	}
}

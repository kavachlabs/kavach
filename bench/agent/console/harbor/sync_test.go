package harbor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAccessKey = "AKIDEXAMPLE0123456789"
	testSecretKey = "r2-secret-key-0123456789abcdef"
	testORKey     = "sk-or-v1-0123456789abcdef0123456789abcdef"
)

// fakeS3 records the PUTs it gets and checks each one's signature.
type fakeS3 struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string][]byte // "/bucket/key" as sent (escaped), to body
	types   map[string]string
	puts    int
	fail    int // answer this many PUTs with 503 first
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{t: t, objects: map[string][]byte{}, types: map[string]string{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	return f, ts
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil { // the client gave up before the whole body arrived
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPut {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if want := authorization(r); r.Header.Get("Authorization") != want {
		f.t.Errorf("Authorization\n got %s\nwant %s", r.Header.Get("Authorization"), want)
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
		return
	}
	if f.fail > 0 {
		f.fail--
		http.Error(w, "try later", http.StatusServiceUnavailable)
		return
	}
	f.puts++
	path := strings.SplitN(r.RequestURI, "?", 2)[0]
	f.objects[path] = body
	f.types[path] = r.Header.Get("Content-Type")
}

// authorization recomputes the header from the request as the server got it.
func authorization(r *http.Request) string {
	amz := r.Header.Get("x-amz-date")
	date := amz[:8]
	canonical := strings.Join([]string{
		r.Method,
		strings.SplitN(r.RequestURI, "?", 2)[0],
		"",
		"host:" + r.Host + "\nx-amz-content-sha256:" + r.Header.Get("x-amz-content-sha256") + "\nx-amz-date:" + amz + "\n",
		"host;x-amz-content-sha256;x-amz-date",
		r.Header.Get("x-amz-content-sha256"),
	}, "\n")
	h := sha256.Sum256([]byte(canonical))
	scope := date + "/auto/s3/aws4_request"
	sig := hex.EncodeToString(hmacSHA256(signingKey(testSecretKey, date, "auto", "s3"), "AWS4-HMAC-SHA256\n"+amz+"\n"+scope+"\n"+hex.EncodeToString(h[:])))
	return "AWS4-HMAC-SHA256 Credential=" + testAccessKey + "/" + scope + ", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=" + sig
}

func (f *fakeS3) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects["/kavach-bench-runs/"+key]
	return b, ok
}

func (f *fakeS3) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

// syncFixture is a runner whose env file holds the R2 credentials and an
// OpenRouter key, and a syncer for a fake S3.
func syncFixture(t *testing.T) (*Syncer, *fakeS3) {
	t.Helper()
	runner, dir := fakeHarbor(t, "")
	env := "R2_ACCOUNT_ID=acct\nR2_ACCESS_KEY_ID=" + testAccessKey + "\nR2_SECRET_ACCESS_KEY=" + testSecretKey + "\nOPENROUTER_API_KEY=" + testORKey + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("R2_BUCKET", "")
	fake, ts := newFakeS3(t)
	s := &Syncer{Runner: runner, Endpoint: ts.URL, Backoff: time.Millisecond}
	s.Reader = &Reader{Dir: runner.JobsDir, Runs: runner, Sync: s}
	return s, fake
}

// finishedJob writes a job folder Harbor has finished.
func finishedJob(t *testing.T, s *Syncer, id string) string {
	t.Helper()
	dir := filepath.Join(s.Runner.JobsDir, id)
	writeFile(t, filepath.Join(dir, "result.json"), `{"started_at": "2026-10-11T10:00:00Z", "finished_at": "2026-10-11T10:01:00Z", "n_total_trials": 0}`)
	return dir
}

// AWS's documented signing key and signature for 20150830/us-east-1/iam.
func TestSigV4Vectors(t *testing.T) {
	key := signingKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam")
	if got := hex.EncodeToString(key); got != "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9" {
		t.Errorf("signing key %s", got)
	}
	toSign := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/iam/aws4_request\nf536975d06c0309214f805bb90ccff089219ecd68b2577efef23edd43b7e1a59"
	if got := hex.EncodeToString(hmacSHA256(key, toSign)); got != "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7" {
		t.Errorf("signature %s", got)
	}
}

// The header aws-sdk-go-v2 v1.37.0 (v4.Signer, s3, DisableURIPathEscaping,
// UNSIGNED-PAYLOAD) computes for the same request.
func TestSignatureMatchesAWSSDK(t *testing.T) {
	c := &S3{Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto", Bucket: "kavach-bench-runs",
		AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC) }}
	path := uriEncode("/kavach-bench-runs/harbor/j1/dir with space/é+x.json")
	if path != "/kavach-bench-runs/harbor/j1/dir%20with%20space/%C3%A9%2Bx.json" {
		t.Fatal(path)
	}
	req, _ := http.NewRequest(http.MethodPut, c.Endpoint+path, http.NoBody)
	if req.URL.RequestURI() != path {
		t.Fatalf("wire path %s", req.URL.RequestURI())
	}
	c.sign(req)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261017/auto/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=41c2c5441fe0d71e3945c229e5ccab110ae8183f873aef8f47a53227bfe98387"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestSyncUploadsFinishedJob(t *testing.T) {
	s, fake := syncFixture(t)
	dir := finishedJob(t, s, "j1")
	writeFile(t, filepath.Join(dir, "trial one", "agent", "out+1.jsonl"), "{\"a\":1}\n")
	writeFile(t, filepath.Join(dir, "job.log"), "hello\n")
	writeFile(t, filepath.Join(dir, "empty.txt"), "")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "must not leave the machine")
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}

	res, err := s.RunOnce(context.Background(), "j1")
	if err != nil || res.Uploaded != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	for key, want := range map[string]string{
		"harbor/j1/job.log":                         "hello\n",
		"harbor/j1/trial%20one/agent/out%2B1.jsonl": "{\"a\":1}\n",
		"harbor/j1/empty.txt":                       "",
	} {
		if got, ok := fake.get(key); !ok || string(got) != want {
			t.Errorf("%s: %q %v", key, got, ok)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for path, body := range fake.objects {
		if strings.Contains(path, "link") || strings.Contains(string(body), "must not leave") {
			t.Errorf("uploaded through a symlink: %s", path)
		}
	}
	if ct := fake.types["/kavach-bench-runs/harbor/j1/job.log"]; ct != "text/plain; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	if ct := fake.types["/kavach-bench-runs/harbor/j1/result.json"]; ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
	st := s.State("j1")
	if st.State != SyncSynced || st.Files != 4 || st.At == nil || st.Skipped != 0 {
		t.Errorf("%+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "sync")); err == nil {
		t.Error("manifest inside the job folder")
	}
	if _, err := os.Stat(s.manifestPath("j1")); err != nil {
		t.Error(err)
	}
}

func TestSyncIsIncremental(t *testing.T) {
	s, fake := syncFixture(t)
	dir := finishedJob(t, s, "j1")
	writeFile(t, filepath.Join(dir, "a.txt"), "one")
	writeFile(t, filepath.Join(dir, "b.txt"), "two")
	ctx := context.Background()
	if res, err := s.RunOnce(ctx, "j1"); err != nil || res.Uploaded != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	if res, err := s.RunOnce(ctx, "j1"); err != nil || res.Uploaded != 0 || res.Unchanged != 3 {
		t.Fatalf("second: %+v %v", res, err)
	}
	if fake.count() != 3 {
		t.Fatalf("%d puts", fake.count())
	}

	// A new console starts with only the manifest.
	s2 := &Syncer{Runner: s.Runner, Reader: s.Reader, Endpoint: s.Endpoint, Backoff: time.Millisecond}
	writeFile(t, filepath.Join(dir, "a.txt"), "ONE")
	later := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(dir, "a.txt"), later, later)
	if res, err := s2.RunOnce(ctx, "j1"); err != nil || res.Uploaded != 1 {
		t.Fatalf("modified: %+v %v", res, err)
	}
	if got, _ := fake.get("harbor/j1/a.txt"); string(got) != "ONE" {
		t.Errorf("a.txt %q", got)
	}

	// Touched but not changed: hashed, not uploaded.
	os.Chtimes(filepath.Join(dir, "b.txt"), later, later)
	if res, err := s2.RunOnce(ctx, "j1"); err != nil || res.Uploaded != 0 {
		t.Fatalf("touched: %+v %v", res, err)
	}

	// Deleted locally: stays in the bucket, and is not an error.
	os.Remove(filepath.Join(dir, "b.txt"))
	if res, err := s2.RunOnce(ctx, "j1"); err != nil || res.Uploaded != 0 {
		t.Fatalf("deleted: %+v %v", res, err)
	}
	if _, ok := fake.get("harbor/j1/b.txt"); !ok {
		t.Error("b.txt was removed from the bucket")
	}
}

func TestSyncSecretGuard(t *testing.T) {
	s, fake := syncFixture(t)
	dir := finishedJob(t, s, "j1")
	writeFile(t, filepath.Join(dir, "fine.txt"), "nothing here")
	writeFile(t, filepath.Join(dir, "exact.log"), "env: OPENROUTER_API_KEY="+testORKey+"\n")
	writeFile(t, filepath.Join(dir, "r2.log"), "secret "+testSecretKey)
	writeFile(t, filepath.Join(dir, "or.txt"), "key sk-or-v1-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123 end")
	writeFile(t, filepath.Join(dir, "ant.txt"), "key sk-ant-api03-ABCDEFGHIJ_KLMNOPQRST-uvwxyz end")
	// A secret across the boundary between two reads.
	pad := strings.Repeat("x", scanChunk-10)
	writeFile(t, filepath.Join(dir, "straddle.txt"), pad+testORKey+"\n")
	writeFile(t, filepath.Join(dir, "short.txt"), "sk-or-v1-tooshort")

	res, err := s.RunOnce(context.Background(), "j1")
	if err != nil || res.Skipped != 5 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, name := range []string{"exact.log", "r2.log", "or.txt", "ant.txt", "straddle.txt"} {
		if _, ok := fake.get("harbor/j1/" + name); ok {
			t.Errorf("%s was uploaded", name)
		}
	}
	for _, name := range []string{"fine.txt", "short.txt"} {
		if _, ok := fake.get("harbor/j1/" + name); !ok {
			t.Errorf("%s was not uploaded", name)
		}
	}
	st := s.State("j1")
	if st.Skipped != 5 || len(st.SkippedFiles) != 5 {
		t.Fatalf("%+v", st)
	}
	reasons := map[string]string{}
	for _, sk := range st.SkippedFiles {
		reasons[sk.Path] = sk.Reason
	}
	if reasons["exact.log"] != "contains the value of OPENROUTER_API_KEY" || reasons["or.txt"] != "matches the pattern sk-or-v1-…" {
		t.Errorf("%v", reasons)
	}
	manifest, _ := os.ReadFile(s.manifestPath("j1"))
	state, _ := json.Marshal(st)
	for _, secret := range []string{testORKey, testSecretKey, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123", "api03-ABCDEFGHIJ"} {
		if strings.Contains(string(state), secret) || strings.Contains(string(manifest), secret) {
			t.Errorf("status or manifest holds %q", secret)
		}
	}

	// Edited to remove the secret, a file goes up on the next sync.
	writeFile(t, filepath.Join(dir, "exact.log"), "clean now\n")
	later := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(dir, "exact.log"), later, later)
	if res, err := s.RunOnce(context.Background(), "j1"); err != nil || res.Uploaded != 1 || res.Skipped != 4 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestSyncRetriesServerErrors(t *testing.T) {
	s, fake := syncFixture(t)
	finishedJob(t, s, "j1")
	fake.fail = 2
	if res, err := s.RunOnce(context.Background(), "j1"); err != nil || res.Uploaded != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	fake.fail = 100
	writeFile(t, filepath.Join(s.Runner.JobsDir, "j1", "new.txt"), "x")
	if _, err := s.RunOnce(context.Background(), "j1"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err %v", err)
	}
	if st := s.State("j1"); st.State != SyncFailed || !strings.Contains(st.Error, "503") {
		t.Errorf("%+v", st)
	}
}

func TestSyncLiveJobIsLeftAlone(t *testing.T) {
	s, fake := syncFixture(t)
	writeFile(t, filepath.Join(s.Runner.JobsDir, "live", "config.json"), "{}")
	if _, err := s.RunOnce(context.Background(), "live"); !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("err %v", err)
	}
	if _, err := s.RunOnce(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if fake.count() != 0 {
		t.Error("uploaded a running job")
	}
}

func TestSyncOffWithoutCredentials(t *testing.T) {
	s, fake := syncFixture(t)
	writeFile(t, s.Runner.EnvFile, "R2_ACCOUNT_ID=acct\n")
	t.Setenv("R2_ACCESS_KEY_ID", "")
	t.Setenv("R2_SECRET_ACCESS_KEY", "")
	finishedJob(t, s, "j1")
	if reason := s.Off(); !strings.Contains(reason, "R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY") {
		t.Errorf("reason %q", reason)
	}
	if st := s.State("j1"); st.State != SyncOff {
		t.Errorf("%+v", st)
	}
	if _, err := s.RunOnce(context.Background(), "j1"); !errors.Is(err, ErrSyncOff) {
		t.Errorf("err %v", err)
	}
	s.Sweep()
	if fake.count() != 0 {
		t.Error("uploaded with no credentials")
	}
	if j, _ := s.Reader.Job("j1"); j.Sync == nil || j.Sync.State != SyncOff {
		t.Errorf("%+v", j.Sync)
	}
}

// A job syncs once its Harbor exits, not while it runs.
func TestSyncAfterHarborExits(t *testing.T) {
	runner, _ := fakeHarbor(t, `
mkdir -p "$JOBS/demo" && echo '{}' > "$JOBS/demo/config.json"
echo "trial output" > "$JOBS/demo/out.txt"
until [ -e "$JOBS/go" ]; do sleep 0.02; done
echo '{"started_at": "2026-10-11T10:00:00Z", "finished_at": "2026-10-11T10:01:00Z", "n_total_trials": 0}' > "$JOBS/demo/result.json"
`)
	t.Setenv("JOBS", runner.JobsDir)
	t.Setenv("R2_BUCKET", "")
	env := "R2_ACCOUNT_ID=acct\nR2_ACCESS_KEY_ID=" + testAccessKey + "\nR2_SECRET_ACCESS_KEY=" + testSecretKey + "\n"
	writeFile(t, runner.EnvFile, env)
	fake, ts := newFakeS3(t)
	s := &Syncer{Runner: runner, Endpoint: ts.URL, Backoff: time.Millisecond}
	s.Reader = &Reader{Dir: runner.JobsDir, Runs: runner, Sync: s}
	runner.OnExit = s.Queue

	if _, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(runner.JobsDir, "demo", "out.txt")); return err == nil })
	s.Sweep()
	time.Sleep(200 * time.Millisecond)
	if fake.count() != 0 {
		t.Fatal("synced while Harbor was running")
	}
	if st := s.State("demo"); st.State != SyncPending {
		t.Errorf("while running: %+v", st)
	}

	writeFile(t, filepath.Join(runner.JobsDir, "go"), "")
	waitFor(t, func() bool { return s.State("demo").State == SyncSynced })
	if got, ok := fake.get("harbor/demo/out.txt"); !ok || string(got) != "trial output\n" {
		t.Errorf("out.txt %q %v", got, ok)
	}
	if _, ok := fake.get("harbor/demo/result.json"); !ok {
		t.Error("result.json not uploaded")
	}
}

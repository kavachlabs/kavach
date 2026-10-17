package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

func clearR2(t *testing.T) {
	for _, k := range []string{"R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY", "R2_BUCKET"} {
		t.Setenv(k, "")
	}
}

func TestStatusSyncMasksKeys(t *testing.T) {
	clearR2(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	_, ts := newServer(t, Config{EnvFile: envPath})
	var st Status
	get(t, ts.URL+"/api/status", &st)
	if st.Sync.Enabled || !strings.Contains(st.Sync.Reason, "R2_ACCOUNT_ID") || st.Sync.Bucket != "kavach-bench-runs" {
		t.Fatalf("%+v", st.Sync)
	}

	os.WriteFile(envPath, []byte("R2_ACCOUNT_ID=0123456789abcdef0123456789abcdef\nR2_ACCESS_KEY_ID=AKIDsecretvalue1234\nR2_SECRET_ACCESS_KEY=topsecretvalue5678\nR2_BUCKET=mine\n"), 0o600)
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, leak := range []string{"0123456789abcdef", "AKIDsecret", "topsecret"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("status leaks %s: %s", leak, raw)
		}
	}
	json.Unmarshal(raw, &st)
	if !st.Sync.Enabled || st.Sync.Bucket != "mine" || len(st.Sync.Keys) != 3 || st.Sync.Keys[2].Masked != "••••5678" || !st.Sync.Keys[0].Set {
		t.Fatalf("%+v", st.Sync)
	}
}

func TestSyncEndpoint(t *testing.T) {
	clearR2(t)
	var puts atomic.Int32
	r2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { puts.Add(1) }))
	t.Cleanup(r2.Close)
	envPath := filepath.Join(t.TempDir(), ".env")
	s, ts := newServer(t, Config{EnvFile: envPath})
	s.syncer.Endpoint = r2.URL

	code, v := post(t, ts.URL+"/api/jobs/oracle-both/sync", "", "", "Origin", ts.URL)
	if code != 409 || !strings.Contains(v["error"].(string), "off") {
		t.Fatalf("while off: %d %v", code, v)
	}
	os.WriteFile(envPath, []byte("R2_ACCOUNT_ID=acct\nR2_ACCESS_KEY_ID=AK\nR2_SECRET_ACCESS_KEY=SK\n"), 0o600)
	s.syncer.Backoff = time.Millisecond

	if code, _ := post(t, ts.URL+"/api/jobs/nope/sync", "", "", "Origin", ts.URL); code != 404 {
		t.Errorf("unknown job: %d", code)
	}
	if code, _ := post(t, ts.URL+"/api/jobs/nop-running/sync", "", "", "Origin", ts.URL); code != 409 {
		t.Errorf("running job: %d", code)
	}
	if code, _ := post(t, ts.URL+"/api/jobs/oracle-both/sync", "", "", "Origin", ts.URL); code != 202 {
		t.Fatalf("finished job: %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	var j harbor.Job
	for ; time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		get(t, ts.URL+"/api/jobs/oracle-both", &j)
		if j.Sync != nil && j.Sync.State == harbor.SyncSynced {
			break
		}
	}
	if j.Sync == nil || j.Sync.State != harbor.SyncSynced || j.Sync.Files == 0 || puts.Load() == 0 {
		t.Fatalf("%+v puts %d", j.Sync, puts.Load())
	}
	var list []harbor.Job
	get(t, ts.URL+"/api/jobs", &list)
	for _, j := range list {
		if j.Sync == nil {
			t.Errorf("%s has no sync state", j.ID)
		}
	}
}

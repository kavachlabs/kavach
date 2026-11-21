package kmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

func writeTestFixture(t *testing.T, path, service, msg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	recs := []journal.Record{
		{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Seq: 0},
		{Type: journal.TypeInput, Seq: 1, Source: "http", Position: "POST /tx", Data: []byte(`{"amount":100}`)},
		{Type: journal.TypeOutput, Seq: 2, Sink: "ledger", Data: []byte(`ok`)},
		{Type: journal.TypeMarker, Seq: 3, Kind: journal.MarkerError, Message: msg},
	}
	if err := journal.WriteFile(path, journal.Meta{Service: service, Start: "genesis"}, recs); err != nil {
		t.Fatal(err)
	}
}

func readNextSSEUpdate(t *testing.T, r *bufio.Reader, timeout time.Duration) LiveEvent {
	t.Helper()
	type result struct {
		ev  LiveEvent
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var eventName string
		var dataLines []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if eventName == "update" && len(dataLines) > 0 {
					var ev LiveEvent
					if uerr := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &ev); uerr != nil {
						ch <- result{err: uerr}
						return
					}
					ch <- result{ev: ev}
					return
				}
				eventName = ""
				dataLines = nil
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read SSE event: %v", res.err)
		}
		return res.ev
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for SSE update event after %s", timeout)
		return LiveEvent{}
	}
}

func TestLiveServerWatchAndEvents(t *testing.T) {
	watchDir := t.TempDir()
	fixturesDir := filepath.Join(watchDir, "fixtures")
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Hidden subdirectory should be ignored unless explicitly passed as a watch dir.
	writeTestFixture(t, filepath.Join(watchDir, ".git", "ignored.kavach"), "ignored", "ignored")

	srv, err := NewLiveServer(LiveConfig{
		WatchDirs: []string{watchDir},
		Interval:  20 * time.Millisecond,
		Keepalive: 50 * time.Millisecond,
		BaseDir:   watchDir,
	})
	if err != nil {
		t.Fatalf("NewLiveServer: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.Start(ctx)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. GET / should include kavach-live meta marker, while static RenderHTML does not.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `<meta name="kavach-live" content="1">`) {
		t.Fatalf("GET / HTML missing kavach-live meta marker")
	}

	var staticBuf bytes.Buffer
	if err := RenderHTML(&staticBuf, &Model{Title: "Static", Stats: map[string]*NodeStats{}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(staticBuf.String(), `<meta name="kavach-live" content="1">`) {
		t.Fatalf("static RenderHTML should not include kavach-live meta tag")
	}

	// 2. GET /api/model starts with 0 fixtures.
	mResp, err := http.Get(ts.URL + "/api/model")
	if err != nil {
		t.Fatalf("GET /api/model: %v", err)
	}
	if got := mResp.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := mResp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := mResp.Header.Get("X-Kavach-Revision"); got != "1" {
		t.Errorf("X-Kavach-Revision = %q, want 1", got)
	}
	var m0 Model
	if err := json.NewDecoder(mResp.Body).Decode(&m0); err != nil {
		mResp.Body.Close()
		t.Fatalf("decode initial model: %v", err)
	}
	mResp.Body.Close()
	if len(m0.Fixtures) != 0 {
		t.Fatalf("initial fixtures = %d, want 0", len(m0.Fixtures))
	}

	// 3. Connect to /api/events and verify initial update event.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	evResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer evResp.Body.Close()
	if got := evResp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("SSE Content-Type = %q, want text/event-stream", got)
	}
	reader := bufio.NewReader(evResp.Body)
	initEv := readNextSSEUpdate(t, reader, 2*time.Second)
	if initEv.Revision != 1 || initEv.Fixtures != 0 {
		t.Fatalf("initial SSE event = %+v, want revision=1 fixtures=0", initEv)
	}

	// 4. Write a valid .kavach fixture and verify SSE emits an update and /api/model reflects it.
	fx1 := filepath.Join(fixturesDir, "svc-run1-000001.kavach")
	writeTestFixture(t, fx1, "payments", "first failure")

	ev2 := readNextSSEUpdate(t, reader, 3*time.Second)
	if ev2.Revision <= initEv.Revision || ev2.Fixtures != 1 {
		t.Fatalf("second SSE event = %+v, want revision>%d fixtures=1", ev2, initEv.Revision)
	}

	mResp2, err := http.Get(ts.URL + "/api/model")
	if err != nil {
		t.Fatal(err)
	}
	var m1 Model
	if err := json.NewDecoder(mResp2.Body).Decode(&m1); err != nil {
		mResp2.Body.Close()
		t.Fatal(err)
	}
	mResp2.Body.Close()
	if len(m1.Fixtures) != 1 || m1.Fixtures[0].Service != "payments" {
		t.Fatalf("updated model fixtures = %+v, want 1 fixture for payments", m1.Fixtures)
	}

	// Unchanged poll should not bump revision.
	changed, err := srv.Refresh()
	if err != nil {
		t.Fatalf("unchanged Refresh: %v", err)
	}
	if changed {
		t.Fatalf("expected unchanged Refresh to return false")
	}

	// 5. Write a truncated/corrupt .kavach file and verify server stays healthy with a warning.
	corruptPath := filepath.Join(fixturesDir, "corrupt.kavach")
	if err := os.WriteFile(corruptPath, []byte("truncated-not-kavach"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev3 := readNextSSEUpdate(t, reader, 3*time.Second)
	if ev3.Revision <= ev2.Revision || ev3.Fixtures != 1 {
		t.Fatalf("corrupt SSE event = %+v, want revision>%d fixtures=1", ev3, ev2.Revision)
	}

	mResp3, err := http.Get(ts.URL + "/api/model")
	if err != nil {
		t.Fatal(err)
	}
	var m2 Model
	if err := json.NewDecoder(mResp3.Body).Decode(&m2); err != nil {
		mResp3.Body.Close()
		t.Fatal(err)
	}
	mResp3.Body.Close()
	if len(m2.Fixtures) != 1 {
		t.Fatalf("fixtures with corrupt file = %d, want 1", len(m2.Fixtures))
	}
	foundWarn := false
	for _, w := range m2.Warnings {
		if strings.Contains(w, "corrupt.kavach") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Fatalf("expected warning mentioning corrupt.kavach, got %v", m2.Warnings)
	}

	// 6. Replace the corrupt file with a valid .kavach file and verify recovery.
	writeTestFixture(t, corruptPath, "orders", "recovered failure")
	ev4 := readNextSSEUpdate(t, reader, 3*time.Second)
	if ev4.Revision <= ev3.Revision || ev4.Fixtures != 2 {
		t.Fatalf("recovery SSE event = %+v, want revision>%d fixtures=2", ev4, ev3.Revision)
	}

	mResp4, err := http.Get(ts.URL + "/api/model")
	if err != nil {
		t.Fatal(err)
	}
	var m3 Model
	if err := json.NewDecoder(mResp4.Body).Decode(&m3); err != nil {
		mResp4.Body.Close()
		t.Fatal(err)
	}
	mResp4.Body.Close()
	if len(m3.Fixtures) != 2 {
		t.Fatalf("recovered fixtures = %d, want 2", len(m3.Fixtures))
	}
	for _, w := range m3.Warnings {
		if strings.Contains(w, "corrupt.kavach") {
			t.Fatalf("did not expect warning for recovered file, got %v", m3.Warnings)
		}
	}
}

func TestLiveServerExplicitDotKavachWatchDir(t *testing.T) {
	base := t.TempDir()
	dotKavach := filepath.Join(base, ".kavach")
	fx := filepath.Join(dotKavach, "fixtures", "svc-r1-000001.kavach")
	writeTestFixture(t, fx, "svc", "inside .kavach")

	srv, err := NewLiveServer(LiveConfig{
		WatchDirs: []string{dotKavach},
		BaseDir:   base,
	})
	if err != nil {
		t.Fatalf("NewLiveServer: %v", err)
	}
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/model", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/model status = %d", rec.Code)
	}
	var m Model
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Fixtures) != 1 {
		t.Fatalf("expected 1 fixture when explicitly watching .kavach dir, got %d", len(m.Fixtures))
	}
}

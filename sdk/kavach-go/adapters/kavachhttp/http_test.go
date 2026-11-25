package kavachhttp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kavachlabs/kavach-go"
	"github.com/kavachlabs/kavach-go/adapters/kavachhttp"
	"github.com/kavachlabs/kavach-go/clock"
	"github.com/kavachlabs/kavach-go/matcher"
	"github.com/kavachlabs/kavach-go/ring"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func decodeStreamFile(t *testing.T, path string) []ring.Record {
	t.Helper()
	var raw []byte
	if r, err := ring.Open(path); err == nil {
		defer r.Close()
		raw, err = r.PollOnce()
		if err != nil {
			t.Fatalf("poll ring: %v", err)
		}
	} else {
		raw, err = os.ReadFile(path)
		if err != nil {
			t.Fatalf("read ring stream: %v", err)
		}
	}
	var dec ring.FrameDecoder
	if _, err := dec.Feed(raw); err != nil {
		t.Fatalf("decode ring stream: %v", err)
	}
	res := dec.FinishEOF()
	var recs []ring.Record
	for _, f := range res.Frames {
		if f.Kind == ring.FrameRecord {
			r, err := ring.DecodeRecord(f.Payload)
			if err != nil {
				t.Fatalf("decode record: %v", err)
			}
			recs = append(recs, r)
		}
	}
	return recs
}

func TestHTTPWrapRecordAndOfflineReplay(t *testing.T) {
	streamPath := filepath.Join(t.TempDir(), "http.stream")
	recKV := kavach.Init(kavach.Config{
		Service:  "orders-api",
		RingPath: streamPath,
		Mode:     kavach.ModeRecord,
		PII:      kavach.ScrubStandard,
	})

	// Mock upstream service for Record mode via an in-memory RoundTripper (no real network needed).
	liveRT := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/rates" {
			t.Fatalf("unexpected upstream path %q", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"rate":1.25}`)),
			Request:    req,
		}, nil
	})

	newApp := func(kv *kavach.Kavach, rt http.RoundTripper) http.Handler {
		client := kavachhttp.WrapClient(kv, &http.Client{Transport: rt})
		mux := http.NewServeMux()
		mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
			var payload struct {
				OrderID string  `json:"order_id"`
				Email   string  `json:"email"`
				Amount  float64 `json:"amount"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://pricing.internal/v1/rates?ccy=USD", nil)
			resp, err := client.Do(req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			var rate struct {
				Rate float64 `json:"rate"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&rate)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"order_id": payload.OrderID,
				"total":    payload.Amount * rate.Rate,
				"ts":       kv.Now(r.Context()).Unix(),
			})
		})
		return kavachhttp.Wrap(kv, mux)
	}

	// 1. Record mode: drive wrapped http.Handler with httptest.ResponseRecorder.
	app := newApp(recKV, liveRT)
	req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(`{"order_id":"ord-1","email":"alice@example.com","amount":100}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token-123")
	req.Header.Set("X-Request-Id", "req-001")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("record status = %d, body = %s", w.Code, w.Body.String())
	}
	if err := recKV.Close(); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	// Verify exact Input, Clock, Gateway, Output records were captured (and PII scrubbed!).
	recs := decodeStreamFile(t, streamPath)
	var (
		inRec   *ring.Record
		clocks  []int64
		gwRecs  []ring.Record
		outRecs []ring.Record
	)
	for i := range recs {
		r := recs[i]
		switch r.Type {
		case ring.RecordInput:
			inRec = &recs[i]
		case ring.RecordClock:
			clocks = append(clocks, r.UnixNanos)
		case ring.RecordGateway:
			gwRecs = append(gwRecs, r)
		case ring.RecordOutput:
			outRecs = append(outRecs, r)
		}
	}
	if inRec == nil || inRec.Source != "http:POST /orders" || inRec.Position != "req-001" {
		t.Fatalf("unexpected input record: %+v", inRec)
	}
	if strings.Contains(string(inRec.Data), "alice@example.com") || strings.Contains(string(inRec.Data), "secret-token-123") {
		t.Fatalf("PII leaked in recorded input: %s", string(inRec.Data))
	}
	if len(clocks) < 2 {
		t.Fatalf("expected >= 2 clock records (step start + handler Now), got %d", len(clocks))
	}
	if len(gwRecs) != 1 || gwRecs[0].Gateway != "http:GET pricing.internal/v1/rates" {
		t.Fatalf("unexpected gateway records: %+v", gwRecs)
	}
	if len(outRecs) != 1 || outRecs[0].Sink != "http:response" {
		t.Fatalf("unexpected output records: %+v", outRecs)
	}

	// 2. Replay mode via ServeOrReplay over stdio pipes with a POISON RoundTripper
	// proving ZERO network calls and ZERO TCP sockets are opened!
	poisonRT := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("NETWORK TOUCHED IN REPLAY MODE: %s %s", req.Method, req.URL.String())
		return nil, errors.New("network forbidden")
	})

	var inBuf bytes.Buffer
	var outBuf bytes.Buffer
	replayKV := kavach.Init(kavach.Config{
		Service: "orders-api",
		Mode:    kavach.ModeReplay,
		Stdin:   &inBuf,
		Stdout:  &outBuf,
	})
	replayApp := newApp(replayKV, poisonRT)

	// Feed SPEC §9 host protocol conversation.
	writeMsg := func(m map[string]any) {
		b, _ := json.Marshal(m)
		inBuf.Write(append(b, '\n'))
	}
	writeMsg(map[string]any{"t": "hello", "protocol": 1, "service": "orders-api", "start": "genesis"})
	writeMsg(map[string]any{
		"t":        "step",
		"seq":      1,
		"source":   inRec.Source,
		"position": inRec.Position,
		"data":     base64.StdEncoding.EncodeToString(inRec.Data),
	})
	// Answer step-start clock, outbound HTTP gateway, and handler clock.
	writeMsg(map[string]any{"t": "clock", "unix_nanos": json.Number(int64Str(clocks[0])).String()})
	writeMsg(map[string]any{"t": "gateway", "response": base64.StdEncoding.EncodeToString(gwRecs[0].Response)})
	writeMsg(map[string]any{"t": "clock", "unix_nanos": json.Number(int64Str(clocks[1])).String()})
	writeMsg(map[string]any{"t": "end"})

	if err := replayKV.ServeOrReplay(replayApp); err != nil {
		t.Fatalf("ServeOrReplay failed: %v", err)
	}

	// Parse host stdout lines and confirm exact output match + outcome: ok.
	lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
	var emittedResp []byte
	sawDoneOK := false
	for _, line := range lines {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("invalid host json %q: %v", line, err)
		}
		if msg["t"] == "emit" && msg["sink"] == "http:response" {
			emittedResp, _ = base64.StdEncoding.DecodeString(msg["data"].(string))
		}
		if msg["t"] == "done" && msg["outcome"] == "ok" {
			sawDoneOK = true
		}
	}
	if !sawDoneOK {
		t.Fatalf("expected done ok, got host log:\n%s", outBuf.String())
	}
	if !bytes.Equal(emittedResp, outRecs[0].Data) {
		t.Fatalf("replayed http:response mismatch:\nwant: %s\ngot:  %s", outRecs[0].Data, emittedResp)
	}
}

func TestHTTPFaultInjectionAndReorderedGateways(t *testing.T) {
	kv := kavach.Init(kavach.Config{
		Service: "checkout-api",
		Mode:    kavach.ModeReplay,
	})
	poisonRT := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("poison transport called in replay")
		return nil, nil
	})
	client := kavachhttp.WrapClient(kv, &http.Client{Transport: poisonRT})

	// Patched handler calls /tax BEFORE /fx (reordered from recording) and before emitting response.
	mux := http.NewServeMux()
	mux.HandleFunc("/quote", func(w http.ResponseWriter, r *http.Request) {
		taxReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://tax.svc/v1/tax", nil)
		taxResp, err := client.Do(taxReq)
		if err != nil {
			http.Error(w, "tax failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer taxResp.Body.Close()

		fxReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://fx.svc/v1/fx", nil)
		fxResp, err := client.Do(fxReq)
		if err != nil {
			http.Error(w, "fx failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer fxResp.Body.Close()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"quoted":true}`))
	})
	wrapped := kavachhttp.WrapWithOptions(kv, mux)

	// Recorded order was /fx THEN /tax.
	recordedGWs := []kavach.GatewayCall{
		{
			Driver:       "http",
			Gateway:      "http:GET fx.svc/v1/fx",
			Op:           "GET fx.svc/v1/fx",
			NormalizedOp: "GET fx.svc/v1/fx",
			KeyArgs:      []string{"fx.svc/v1/fx"},
			Response:     []byte(`{"status_code":200,"body_json":{"rate":1.1}}`),
		},
		{
			Driver:       "http",
			Gateway:      "http:GET tax.svc/v1/tax",
			Op:           "GET tax.svc/v1/tax",
			NormalizedOp: "GET tax.svc/v1/tax",
			KeyArgs:      []string{"tax.svc/v1/tax"},
			Response:     []byte(`{"status_code":200,"body_json":{"pct":0.2}}`),
		},
	}

	in := kavach.Input{
		Source: "http:GET /quote",
		Data:   []byte(`{"method":"GET","path":"/quote","has_clock":true}`),
	}
	fixedCtx := clock.WithProvider(context.Background(), &clock.ReplayProvider{
		LastClock: time.Unix(1700000000, 0).UTC().UnixNano(),
		Lenient:   true,
	})

	// Attempt 1: inject ETIMEDOUT on /tax -> returns 503 error.
	tbl1 := matcher.NewStepTable(1, recordedGWs, nil)
	tbl1.InjectFault(kavach.FaultRule{
		Driver:    "http",
		Gateway:   "http:GET tax.svc/v1/tax",
		ErrString: kavach.CodeETIMEDOUT,
		Once:      true,
	})
	_, err := kv.RunStepWithTable(fixedCtx, in, tbl1, wrapped)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected 503 error on injected ETIMEDOUT, got %v", err)
	}

	// Attempt 2: clean replay with reordered /tax before /fx -> KeyedTemplate succeeds!
	tbl2 := matcher.NewStepTable(2, recordedGWs, nil)
	sc2, err := kv.RunStepWithTable(fixedCtx, in, tbl2, wrapped)
	if err != nil {
		t.Fatalf("expected reordered replay to succeed, got %v", err)
	}
	if tbl2.ReorderedCount() == 0 {
		t.Fatalf("expected reordered_gateway telemetry > 0")
	}
	if len(sc2.Outputs()) != 1 || sc2.Outputs()[0].Sink != "http:response" {
		t.Fatalf("expected 1 http:response output, got %+v", sc2.Outputs())
	}
}

func TestHTTPBinaryBodyRoundTrip(t *testing.T) {
	kv := kavach.Init(kavach.Config{Service: "bin-svc", Mode: kavach.ModeReplay})
	binPayload := []byte{0x00, 0x01, 0xfe, 0xff, 'O', 'K'}
	wrapped := kavachhttp.WrapWithOptions(kv, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, binPayload) {
			t.Errorf("binary req body mismatch: got %x want %x", got, binPayload)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(binPayload)
	}))

	reqEnv, _ := json.Marshal(kavachhttp.RecordedRequest{
		Method: http.MethodPost,
		Path:   "/blob",
		Body:   binPayload,
	})
	tbl := matcher.NewStepTable(1, nil, nil)
	sc, err := kv.RunStepWithTable(context.Background(), kavach.Input{Source: "http:POST /blob", Data: reqEnv}, tbl, wrapped)
	if err != nil {
		t.Fatalf("binary RunStep failed: %v", err)
	}
	var recResp kavachhttp.RecordedResponse
	if err := json.Unmarshal(sc.Outputs()[0].Data, &recResp); err != nil {
		t.Fatalf("unmarshal RecordedResponse: %v", err)
	}
	if !bytes.Equal(recResp.Body, binPayload) {
		t.Fatalf("binary resp mismatch: got %x want %x", recResp.Body, binPayload)
	}
}

func TestHTTPTransportPOSTBodyHashDistinguishesSameURLQueries(t *testing.T) {
	// ClickHouse HTTP pattern: multiple POST requests to the exact same URL/query string
	// with different SQL statements in req.Body must not collide on Record or reordered Replay.
	ringPath := filepath.Join(t.TempDir(), "ch.shm")
	recKV := kavach.Init(kavach.Config{Service: "openmeter", RingPath: ringPath, Mode: kavach.ModeRecord})

	liveRT := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		sql := string(body)
		respBody := `{"rows":0}`
		switch {
		case strings.Contains(sql, "FROM events WHERE subject = 'a'"):
			respBody = `{"rows":10}`
		case strings.Contains(sql, "FROM events WHERE subject = 'b'"):
			respBody = `{"rows":25}`
		default:
			t.Fatalf("unexpected body in liveRT: %q", sql)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(respBody)),
			Request:    req,
		}, nil
	})
	recClient := kavachhttp.WrapClient(recKV, &http.Client{Transport: liveRT})

	doCH := func(ctx context.Context, client *http.Client, sql string) string {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://clickhouse:8123/?default_format=Native", strings.NewReader(sql))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("CH POST %q failed: %v", sql, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	stepCtx, sc := recKV.BeginStep(context.Background(), kavach.Input{Source: "cmd:meter", Data: []byte(`{}`)})
	if got := doCH(stepCtx, recClient, "SELECT sum(v) FROM events WHERE subject = 'a'"); got != `{"rows":10}` {
		t.Fatalf("record query a = %s", got)
	}
	if got := doCH(stepCtx, recClient, "SELECT sum(v) FROM events WHERE subject = 'b'"); got != `{"rows":25}` {
		t.Fatalf("record query b = %s", got)
	}
	_ = sc.End(nil)
	_ = recKV.Close()

	recs := decodeStreamFile(t, ringPath)
	var gws []kavach.GatewayCall
	for _, r := range recs {
		if r.Type == ring.RecordGateway {
			gc := matcher.ParseGatewayRequest(r.Gateway, r.Request)
			gc.Response = r.Response
			gws = append(gws, gc)
		}
	}
	if len(gws) != 2 {
		t.Fatalf("expected 2 CH gateway records, got %d", len(gws))
	}
	if gws[0].OpFingerprint == gws[1].OpFingerprint {
		t.Fatalf("distinct POST bodies collided with same OpFingerprint %q (keyArgs=%v vs %v)", gws[0].OpFingerprint, gws[0].KeyArgs, gws[1].KeyArgs)
	}

	// Replay offline in REVERSE order (query b before query a) -> must return exact per-query responses!
	replayKV := kavach.Init(kavach.Config{Service: "openmeter", Mode: kavach.ModeReplay})
	poisonRT := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("network touched in replay")
		return nil, nil
	})
	replayClient := kavachhttp.WrapClient(replayKV, &http.Client{Transport: poisonRT})

	tbl := matcher.NewStepTable(1, gws, nil)
	_, err := replayKV.RunStepWithTable(context.Background(), kavach.Input{Source: "cmd:meter", Data: []byte(`{}`)}, tbl, kavach.StepFunc(func(ctx context.Context, _ kavach.Input) error {
		if got := doCH(ctx, replayClient, "SELECT sum(v) FROM events WHERE subject = 'b'"); got != `{"rows":25}` {
			t.Fatalf("reordered replay query b = %s, want {\"rows\":25}", got)
		}
		if got := doCH(ctx, replayClient, "SELECT sum(v) FROM events WHERE subject = 'a'"); got != `{"rows":10}` {
			t.Fatalf("reordered replay query a = %s, want {\"rows\":10}", got)
		}
		return nil
	}))
	if err != nil {
		t.Fatalf("reordered CH replay failed: %v", err)
	}
	if tbl.ReorderedCount() == 0 {
		t.Fatalf("expected reordered_gateway > 0")
	}
}

func TestHTTPWithErrorClassifier(t *testing.T) {
	kv := kavach.Init(kavach.Config{Service: "blnk", Mode: kavach.ModeReplay})
	classifier := func(status int, body []byte) error {
		if status == http.StatusConflict {
			return nil // idempotent duplicate reference
		}
		if status == http.StatusBadRequest && strings.Contains(string(body), "INSUFFICIENT_FUNDS") {
			return errors.New("TXN_INSUFFICIENT_FUNDS")
		}
		if status >= 500 {
			return errors.New("server error")
		}
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/tx", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("case") {
		case "conflict":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"TXN_DUPLICATE_REFERENCE"}`))
		case "insuff":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"TXN_INSUFFICIENT_FUNDS ref=123"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	})
	wrapped := kavachhttp.WrapWithOptions(kv, mux, kavachhttp.WithErrorClassifier(classifier))

	// 409 Conflict classified as nil error.
	if _, err := kv.RunStepWithTable(context.Background(), kavach.Input{
		Source: "http:POST /tx",
		Data:   []byte(`{"method":"POST","path":"/tx","raw_query":"case=conflict"}`),
	}, matcher.NewStepTable(1, nil, nil), wrapped); err != nil {
		t.Fatalf("409 should classify as nil, got %v", err)
	}

	// 400 Insufficient funds classified as canonical TXN_INSUFFICIENT_FUNDS error.
	_, err := kv.RunStepWithTable(context.Background(), kavach.Input{
		Source: "http:POST /tx",
		Data:   []byte(`{"method":"POST","path":"/tx","raw_query":"case=insuff"}`),
	}, matcher.NewStepTable(2, nil, nil), wrapped)
	if err == nil || err.Error() != "TXN_INSUFFICIENT_FUNDS" {
		t.Fatalf("400 expected canonical TXN_INSUFFICIENT_FUNDS, got %v", err)
	}
}

func int64Str(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Package kavachhttp provides zero-dependency net/http adapters for Kavach v0.2:
//   - Wrap / Middleware for inbound HTTP servers (capturing Input, Clock, and http:response Output,
//     and implementing kavach.StepRunner for socket-free deterministic Replay via httptest.ResponseRecorder).
//   - WrapTransport / WrapClient for outbound HTTP clients (recording/replaying Gateway calls offline).
package kavachhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/kavachlabs/kavach-go"
)

const defaultMaxBodyBytes = 4 << 20 // 4 MiB

// Option configures WrapWithOptions or Middleware.
type Option func(*options)

type options struct {
	sourceFunc      func(*http.Request) string
	errorStatus     int
	errorClassifier func(status int, body []byte) error
	maxBodyBytes    int64
}

func defaultOptions() options {
	return options{
		errorStatus:  http.StatusInternalServerError,
		maxBodyBytes: defaultMaxBodyBytes,
	}
}

// WithSourceFunc overrides the Input.Source format (default: "http:<METHOD> <PATH>").
func WithSourceFunc(fn func(*http.Request) string) Option {
	return func(o *options) {
		o.sourceFunc = fn
	}
}

// WithErrorStatus sets the minimum HTTP status code treated as a step error (default: 500).
// Set <= 0 to never treat HTTP status codes as step errors.
func WithErrorStatus(minStatus int) Option {
	return func(o *options) {
		o.errorStatus = minStatus
	}
}

// WithErrorClassifier configures a custom classifier invoked on both ServeHTTP
// and RunStep to turn (status, responseBody) into the step error (or nil).
// When set, it takes precedence over WithErrorStatus.
func WithErrorClassifier(fn func(status int, body []byte) error) Option {
	return func(o *options) {
		o.errorClassifier = fn
	}
}

// WithMaxBodyBytes caps request/response body bytes buffered for recording (default: 4 MiB).
func WithMaxBodyBytes(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.maxBodyBytes = n
		}
	}
}

// RecordedRequest is the JSON wire envelope recorded in Input.Data for inbound HTTP requests.
// JSON bodies are stored inline in BodyJSON so Kavach PII scrubbing and JSON mutation work directly;
// non-JSON/binary bodies are stored losslessly in Body.
type RecordedRequest struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	RawQuery string            `json:"raw_query,omitempty"`
	Host     string            `json:"host,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	BodyJSON json.RawMessage   `json:"body_json,omitempty"`
	Body     []byte            `json:"body,omitempty"`
	HasClock bool              `json:"has_clock,omitempty"`
}

// RecordedResponse is the JSON wire envelope emitted to the "http:response" Output sink.
type RecordedResponse struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers,omitempty"`
	BodyJSON json.RawMessage   `json:"body_json,omitempty"`
	Body     []byte            `json:"body,omitempty"`
}

// Handler wraps an http.Handler to record inbound HTTP steps in ModeRecord and
// implements kavach.StepRunner so kv.ServeOrReplay drives it offline via
// httptest.ResponseRecorder without opening any TCP socket.
type Handler struct {
	kv   *kavach.Kavach
	next http.Handler
	opts options
}

var (
	_ http.Handler      = (*Handler)(nil)
	_ kavach.StepRunner = (*Handler)(nil)
)

// Wrap wraps h with Kavach HTTP step recording and replay support.
// The returned http.Handler also implements kavach.StepRunner for kv.ServeOrReplay.
func Wrap(kv *kavach.Kavach, h http.Handler) http.Handler {
	return WrapWithOptions(kv, h)
}

// WrapWithOptions wraps h with optional configuration and returns the concrete *Handler.
func WrapWithOptions(kv *kavach.Kavach, h http.Handler, opts ...Option) *Handler {
	cfg := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &Handler{
		kv:   kv,
		next: h,
		opts: cfg,
	}
}

// Middleware returns a standard net/http middleware compatible with chi, gorilla/mux, etc.
func Middleware(kv *kavach.Kavach, opts ...Option) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return WrapWithOptions(kv, next, opts...)
	}
}

// ServeHTTP implements http.Handler.
// In ModeOff it delegates directly with zero overhead.
// In ModeRecord it captures Input, Clock, and Output ("http:response") around h.next.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.next == nil {
		return
	}
	if h.kv == nil || h.kv.IsOff() {
		h.next.ServeHTTP(w, r)
		return
	}

	// If already inside an active Replay step (e.g. re-entered), execute directly.
	if h.kv.IsReplay() {
		if _, ok := h.kv.ActiveStep(r.Context()); ok {
			h.next.ServeHTTP(w, r)
			return
		}
	}

	body, err := readAndRestoreRequestBody(r, h.opts.maxBodyBytes)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	source := h.sourceFor(r)
	flatHdrs := flattenHeaders(r.Header)
	recReq := RecordedRequest{
		Method:   r.Method,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Host:     r.Host,
		Headers:  h.kv.ScrubHeaders(flatHdrs),
		HasClock: true,
	}
	setRecordedBody(&recReq.BodyJSON, &recReq.Body, body)
	reqData, _ := json.Marshal(recReq)

	in := kavach.Input{
		Source:   source,
		Position: r.Header.Get("X-Request-Id"),
		Headers:  flatHdrs,
		Data:     reqData,
	}

	stepCtx, sc := h.kv.BeginStep(r.Context(), in)
	// Capture deterministic step-start clock record (RecordType::Clock).
	_ = h.kv.Now(stepCtx)

	rc := newResponseCapture(w, h.opts.maxBodyBytes)
	panicked := true
	defer func() {
		if panicked {
			pv := recover()
			_ = sc.End(fmt.Errorf("panic: %v", pv))
			panic(pv)
		}
	}()

	h.next.ServeHTTP(rc, r.WithContext(stepCtx))
	panicked = false

	respOut := buildResponseOutput(rc.status, rc.Header(), rc.body.Bytes())
	sc.RecordOutput(kavach.Output{
		Sink: "http:response",
		Data: respOut,
	})

	var stepErr error
	if h.opts.errorClassifier != nil {
		stepErr = h.opts.errorClassifier(rc.status, rc.body.Bytes())
	} else if h.opts.errorStatus > 0 && rc.status >= h.opts.errorStatus {
		stepErr = fmt.Errorf("http %d: %s", rc.status, strings.TrimSpace(rc.body.String()))
	}
	_ = sc.End(stepErr)
}

// RunStep implements kavach.StepRunner, replaying one recorded/mutated HTTP request
// deterministically via httptest.ResponseRecorder without opening any network socket.
func (h *Handler) RunStep(ctx context.Context, in kavach.Input) error {
	if h.next == nil {
		return nil
	}
	req, hasClock := buildReplayRequest(ctx, in)
	if hasClock && h.kv != nil {
		_ = h.kv.Now(ctx)
	}

	rec := httptest.NewRecorder()
	h.next.ServeHTTP(rec, req)

	respOut := buildResponseOutput(rec.Code, rec.Header(), rec.Body.Bytes())
	if h.kv != nil {
		h.kv.RecordOutput(ctx, kavach.Output{
			Sink: "http:response",
			Data: respOut,
		})
	}
	if h.opts.errorClassifier != nil {
		return h.opts.errorClassifier(rec.Code, rec.Body.Bytes())
	}
	if h.opts.errorStatus > 0 && rec.Code >= h.opts.errorStatus {
		return fmt.Errorf("http %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return nil
}

func (h *Handler) sourceFor(r *http.Request) string {
	if h.opts.sourceFunc != nil {
		if s := h.opts.sourceFunc(r); s != "" {
			return s
		}
	}
	return "http:" + r.Method + " " + r.URL.Path
}

func buildReplayRequest(ctx context.Context, in kavach.Input) (*http.Request, bool) {
	method, path := http.MethodPost, "/"
	if rest, ok := strings.CutPrefix(in.Source, "http:"); ok {
		parts := strings.SplitN(strings.TrimSpace(rest), " ", 2)
		if len(parts) == 2 {
			method, path = parts[0], parts[1]
		} else if rest != "" {
			path = rest
		}
	}

	rawQuery := ""
	host := ""
	headers := in.Headers
	body := in.Data
	hasClock := false

	var env struct {
		Method   string            `json:"method"`
		Path     string            `json:"path"`
		URL      string            `json:"url"`
		RawQuery string            `json:"raw_query"`
		Host     string            `json:"host"`
		Headers  map[string]string `json:"headers"`
		BodyJSON json.RawMessage   `json:"body_json"`
		Body     []byte            `json:"body"`
		HasClock bool              `json:"has_clock"`
	}
	if len(in.Data) > 0 && in.Data[0] == '{' && json.Unmarshal(in.Data, &env) == nil &&
		(env.Method != "" || env.Path != "" || env.URL != "") {
		if env.Method != "" {
			method = env.Method
		}
		if env.Path != "" {
			path = env.Path
		} else if env.URL != "" {
			path = env.URL
		}
		rawQuery = env.RawQuery
		host = env.Host
		if len(env.Headers) > 0 {
			headers = env.Headers
		}
		hasClock = env.HasClock
		switch {
		case len(env.BodyJSON) > 0:
			body = env.BodyJSON
		case env.Body != nil:
			body = env.Body
		default:
			body = nil
		}
	}

	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		path = "/" + path
	}
	target := path
	if rawQuery != "" && !strings.Contains(target, "?") {
		target += "?" + rawQuery
	}

	req := httptest.NewRequest(method, target, bytes.NewReader(body)).WithContext(ctx)
	if host != "" {
		req.Host = host
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, hasClock
}

func buildResponseOutput(status int, hdr http.Header, body []byte) []byte {
	if status == 0 {
		status = http.StatusOK
	}
	rec := RecordedResponse{
		Status:  status,
		Headers: flattenHeaders(hdr),
	}
	setRecordedBody(&rec.BodyJSON, &rec.Body, body)
	b, _ := json.Marshal(rec)
	return b
}

func setRecordedBody(dstJSON *json.RawMessage, dstBytes *[]byte, body []byte) {
	if len(body) == 0 {
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
		var compact bytes.Buffer
		if json.Compact(&compact, trimmed) == nil {
			*dstJSON = json.RawMessage(compact.Bytes())
			return
		}
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	*dstBytes = cp
}

func readAndRestoreRequestBody(r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	orig := r.Body
	defer orig.Close()
	if maxBytes <= 0 {
		maxBytes = defaultMaxBodyBytes
	}
	data, err := io.ReadAll(io.LimitReader(orig, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		rest, _ := io.ReadAll(orig)
		full := append(data, rest...)
		r.Body = io.NopCloser(bytes.NewReader(full))
		return data[:maxBytes], nil
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func flattenHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) > 0 {
			out[k] = strings.Join(vals, ", ")
		}
	}
	return out
}

type responseCapture struct {
	w           http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
	maxBytes    int64
}

func newResponseCapture(w http.ResponseWriter, maxBytes int64) *responseCapture {
	return &responseCapture{
		w:        w,
		status:   http.StatusOK,
		maxBytes: maxBytes,
	}
}

func (rc *responseCapture) Header() http.Header { return rc.w.Header() }

func (rc *responseCapture) WriteHeader(code int) {
	if !rc.wroteHeader {
		rc.status = code
		rc.wroteHeader = true
	}
	rc.w.WriteHeader(code)
}

func (rc *responseCapture) Write(b []byte) (int, error) {
	if !rc.wroteHeader {
		rc.wroteHeader = true
	}
	if rc.maxBytes <= 0 || int64(rc.body.Len()) < rc.maxBytes {
		rem := int64(len(b))
		if rc.maxBytes > 0 && int64(rc.body.Len())+rem > rc.maxBytes {
			rem = rc.maxBytes - int64(rc.body.Len())
		}
		if rem > 0 {
			rc.body.Write(b[:rem])
		}
	}
	return rc.w.Write(b)
}

func (rc *responseCapture) Flush() {
	if f, ok := rc.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (rc *responseCapture) Unwrap() http.ResponseWriter {
	return rc.w
}

package kavachhttp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kavachlabs/kavach-go"
)

// Transport wraps an http.RoundTripper to record outbound HTTP client calls as
// Gateway records ("http:<METHOD> <host><path>") and replay them 100% offline.
type Transport struct {
	kv           *kavach.Kavach
	base         http.RoundTripper
	maxBodyBytes int64
}

var _ http.RoundTripper = (*Transport)(nil)

// WrapTransport returns an http.RoundTripper that records outbound HTTP calls in
// ModeRecord and serves recorded responses (or injected faults) offline in ModeReplay.
// If rt is nil, http.DefaultTransport is used outside ModeReplay.
func WrapTransport(kv *kavach.Kavach, rt http.RoundTripper) http.RoundTripper {
	return &Transport{
		kv:           kv,
		base:         rt,
		maxBodyBytes: defaultMaxBodyBytes,
	}
}

// WrapClient clones c (or allocates a new *http.Client if nil) with its Transport
// wrapped via WrapTransport.
func WrapClient(kv *kavach.Kavach, c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{Transport: WrapTransport(kv, http.DefaultTransport)}
	}
	cp := *c
	cp.Transport = WrapTransport(kv, c.Transport)
	return &cp
}

type recordedClientResponse struct {
	StatusCode int                 `json:"status_code"`
	Status     string              `json:"status,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	BodyJSON   json.RawMessage     `json:"body_json,omitempty"`
	Body       []byte              `json:"body,omitempty"`
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kavachhttp: nil request")
	}
	if t.kv == nil || t.kv.IsOff() {
		return t.baseRoundTripper().RoundTrip(req)
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(method)
	hostPath := req.URL.Host + req.URL.Path
	op := method + " " + hostPath

	keyArgs := []string{hostPath}
	if req.URL.RawQuery != "" {
		keyArgs = append(keyArgs, req.URL.RawQuery)
	}
	if req.Body != nil && req.Body != http.NoBody {
		if reqBody, readErr := readAndRestoreRequestBody(req, t.maxBodyBytes); readErr == nil && len(reqBody) > 0 {
			sum := sha256.Sum256(reqBody)
			keyArgs = append(keyArgs, "body:"+hex.EncodeToString(sum[:8]))
		}
	}

	call := kavach.GatewayCall{
		Driver:       "http",
		Gateway:      "http:" + op,
		Op:           op,
		NormalizedOp: op,
		KeyArgs:      keyArgs,
	}

	if t.kv.IsReplay() {
		respBytes, err := t.kv.ReplayGateway(req.Context(), call)
		if err != nil {
			return nil, err
		}
		return decodeClientResponse(req, respBytes)
	}

	// Record mode: execute real outbound HTTP request, capture response/error, never block.
	resp, err := t.baseRoundTripper().RoundTrip(req)
	if err != nil {
		call.Error = err.Error()
		t.kv.RecordGateway(req.Context(), call)
		return nil, err
	}

	body, readErr := readAndRestoreResponseBody(resp, t.maxBodyBytes)
	if readErr != nil {
		call.Error = readErr.Error()
		t.kv.RecordGateway(req.Context(), call)
		return resp, nil
	}

	recResp := recordedClientResponse{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Headers:    cloneHTTPHeader(resp.Header),
	}
	setRecordedBody(&recResp.BodyJSON, &recResp.Body, body)
	encoded, _ := json.Marshal(recResp)
	call.Response = encoded
	t.kv.RecordGateway(req.Context(), call)
	return resp, nil
}

func (t *Transport) baseRoundTripper() http.RoundTripper {
	if t.base != nil {
		return t.base
	}
	return http.DefaultTransport
}

func decodeClientResponse(req *http.Request, data []byte) (*http.Response, error) {
	if len(data) == 0 {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          io.NopCloser(bytes.NewReader(nil)),
			ContentLength: 0,
			Request:       req,
		}, nil
	}

	var rec recordedClientResponse
	if err := json.Unmarshal(data, &rec); err != nil {
		// Graceful fallback if raw response payload was stored directly.
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          io.NopCloser(bytes.NewReader(data)),
			ContentLength: int64(len(data)),
			Request:       req,
		}, nil
	}

	statusCode := rec.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	status := rec.Status
	if status == "" {
		status = fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))
	}
	hdr := make(http.Header, len(rec.Headers))
	for k, vals := range rec.Headers {
		cp := make([]string, len(vals))
		copy(cp, vals)
		hdr[k] = cp
	}

	var body []byte
	switch {
	case len(rec.BodyJSON) > 0:
		body = rec.BodyJSON
	case len(rec.Body) > 0:
		body = rec.Body
	}
	return &http.Response{
		StatusCode:    statusCode,
		Status:        status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

func readAndRestoreResponseBody(resp *http.Response, maxBytes int64) ([]byte, error) {
	if resp.Body == nil || resp.Body == http.NoBody {
		return nil, nil
	}
	orig := resp.Body
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
		resp.Body = io.NopCloser(bytes.NewReader(full))
		return data[:maxBytes], nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func cloneHTTPHeader(h http.Header) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, vals := range h {
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}

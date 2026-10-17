package harbor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// S3 puts objects into one bucket of an S3-compatible store (Cloudflare R2),
// signing requests with AWS Signature Version 4. It is the one call the sync
// needs, so it is written here rather than taken from an SDK, which would be
// the console's only large dependency.
//
// The payload is not hashed: x-amz-content-sha256 is UNSIGNED-PAYLOAD, which
// S3 and R2 accept over TLS, so a file streams from disk in one pass.
type S3 struct {
	Endpoint  string // https://<account>.r2.cloudflarestorage.com
	Region    string // auto for R2
	Bucket    string
	AccessKey string
	SecretKey string
	Client    *http.Client
	// Backoff is the wait before the first retry; it doubles. Default 500ms.
	Backoff time.Duration
	now     func() time.Time
}

const s3Attempts = 4

// errChanged is returned when a file's content is not what was scanned.
var errChanged = errors.New("file changed while uploading")

// Put uploads the file at path, size bytes, as key. It retries network
// errors, 429 and 5xx; any other status is final. The body is held to
// wantSum, the SHA-256 of what was scanned: if the file no longer hashes to
// it, the request is aborted before its last byte, so the store never
// commits the object, and Put returns errChanged.
func (c *S3) Put(ctx context.Context, key, path string, size int64, contentType, wantSum string) error {
	wait := c.Backoff
	if wait == 0 {
		wait = 500 * time.Millisecond
	}
	var err error
	for attempt := 0; attempt < s3Attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}
		var retry bool
		if retry, err = c.put(ctx, key, path, size, contentType, wantSum); !retry {
			return err
		}
	}
	return err
}

func (c *S3) put(ctx context.Context, key, path string, size int64, contentType, wantSum string) (retry bool, err error) {
	var body io.Reader = http.NoBody
	if size > 0 {
		f, err := os.Open(path)
		if err != nil {
			return false, err
		}
		defer f.Close()
		body = &guarded{src: io.LimitReader(f, size), want: wantSum, h: sha256.New(), buf: make([]byte, 64<<10)}
	}
	target := strings.TrimRight(c.Endpoint, "/") + uriEncode("/"+c.Bucket+"/"+key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return false, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	c.sign(req)
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errChanged) {
			return false, errChanged
		}
		return ctx.Err() == nil, fmt.Errorf("PUT %s: %s", key, shortErr(err, req.URL.Host))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	// Only the error's code and message are kept: the rest of the body
	// echoes the request, access key ID included.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct{ Code, Message string }
	xml.Unmarshal(raw, &e)
	detail := strings.TrimSpace(e.Code + " " + e.Message)
	if len(detail) > 200 {
		detail = detail[:200]
	}
	err = fmt.Errorf("PUT %s: %s %s", key, resp.Status, detail)
	return resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests, err
}

// shortErr describes a transport error without the endpoint, whose host
// holds the account ID.
func shortErr(err error, host string) string {
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.As(err, &dns):
		err = fmt.Errorf("lookup: %s", dns.Err)
	case errors.As(err, &op):
		err = fmt.Errorf("%s: %v", op.Op, op.Err)
	default:
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) {
			err = ue.Unwrap()
		}
	}
	msg := strings.ReplaceAll(err.Error(), host, "r2")
	if h, _, e := net.SplitHostPort(host); e == nil {
		msg = strings.ReplaceAll(msg, h, "r2")
	}
	return msg
}

// guarded streams src while hashing it, and holds back its last byte until
// the hash is known to match want: a file that changed since it was scanned
// fails the request before the store has the whole body.
type guarded struct {
	src  io.Reader
	want string
	h    hash.Hash
	buf  []byte
	out  []byte
	held []byte
	eof  bool
}

func (g *guarded) Read(p []byte) (int, error) {
	for len(g.out) == 0 {
		if g.eof {
			return 0, io.EOF
		}
		n, err := g.src.Read(g.buf)
		if n > 0 {
			g.h.Write(g.buf[:n])
			all := append(append([]byte(nil), g.held...), g.buf[:n]...)
			g.out, g.held = all[:len(all)-1], all[len(all)-1:]
		}
		if err == io.EOF {
			if hex.EncodeToString(g.h.Sum(nil)) != g.want {
				return 0, errChanged
			}
			g.out, g.held, g.eof = append(g.out, g.held...), nil, true
		} else if err != nil {
			return 0, err
		}
	}
	n := copy(p, g.out)
	g.out = g.out[n:]
	return n, nil
}

// sign adds the Authorization header and the headers it covers.
func (c *S3) sign(req *http.Request) {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	amzDate := now().UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	region := c.Region
	if region == "" {
		region = "auto"
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	const signed = "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		"",
		"host:" + req.URL.Host + "\nx-amz-content-sha256:UNSIGNED-PAYLOAD\nx-amz-date:" + amzDate + "\n",
		signed,
		"UNSIGNED-PAYLOAD",
	}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, date, region, "s3"), toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.AccessKey, scope, signed, sig))
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// uriEncode percent-encodes everything but the unreserved characters and
// "/", as SigV4 canonical paths require.
func uriEncode(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

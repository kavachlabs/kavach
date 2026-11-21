package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kavachlabs/kavach/internal/kmap"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMapLiveValidation(t *testing.T) {
	watchDir := t.TempDir()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "zero interval",
			args: []string{"--watch", watchDir, "--interval", "0s"},
			want: "--interval must be positive",
		},
		{
			name: "negative interval",
			args: []string{"--watch", watchDir, "--interval", "-5ms"},
			want: "--interval must be positive",
		},
		{
			name: "watch with json",
			args: []string{"--watch", watchDir, "--json"},
			want: "--watch cannot be combined with --json",
		},
		{
			name: "watch with out",
			args: []string{"--watch", watchDir, "-o", "out.html"},
			want: "--watch cannot be combined with -o/--out",
		},
		{
			name: "watch with new without bin",
			args: []string{"--watch", watchDir, "--new", "./candidate"},
			want: "--new requires --bin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := cmdMapWithContext(context.Background(), newUI(&out, true), tc.args, &out, &errb)
			if code != exitUsage {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, errb.String())
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Fatalf("stderr %q missing %q", errb.String(), tc.want)
			}
		})
	}
}

func TestMapLiveServerStartAndCancel(t *testing.T) {
	watchDir := t.TempDir()
	fixCopy := filepath.Join(watchDir, "fixtures", "null-amount.kavach")
	if err := os.MkdirAll(filepath.Dir(fixCopy), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixCopy, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out, errb syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- cmdMapWithContext(
			ctx,
			newUI(&out, true),
			[]string{"--watch", watchDir, "--listen", "127.0.0.1:0", "--interval", "25ms"},
			&out,
			&errb,
		)
	}()

	urlRe := regexp.MustCompile(`http://127\.0\.0\.1:\d+`)
	var liveURL string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m := urlRe.FindString(out.String()); m != "" {
			liveURL = m
			break
		}
		select {
		case code := <-done:
			t.Fatalf("cmdMapWithContext exited early with %d; stdout=%s stderr=%s", code, out.String(), errb.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if liveURL == "" {
		cancel()
		t.Fatalf("timed out waiting for live URL in stdout: %q (stderr: %q)", out.String(), errb.String())
	}

	resp, err := http.Get(liveURL + "/api/model")
	if err != nil {
		cancel()
		t.Fatalf("GET /api/model: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET /api/model status = %d", resp.StatusCode)
	}
	var m kmap.Model
	if err := json.Unmarshal(body, &m); err != nil {
		cancel()
		t.Fatalf("decode /api/model: %v", err)
	}
	if len(m.Fixtures) != 1 {
		cancel()
		t.Fatalf("expected 1 fixture in live model, got %d", len(m.Fixtures))
	}

	cancel()
	select {
	case code := <-done:
		if code != exitPass {
			t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitPass, errb.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for live map server to shut down")
	}
}

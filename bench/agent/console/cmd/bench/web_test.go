package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

func stubBrowser(t *testing.T, open func(url string) error) {
	t.Helper()
	prev := openBrowser
	openBrowser = open
	t.Cleanup(func() { openBrowser = prev })
}

func TestWebReusesServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	var opened string
	stubBrowser(t, func(url string) error { opened = url; return nil })

	var out, errb bytes.Buffer
	if code := run([]string{"web", "-port", u.Port(), "-root", "/nonexistent"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if want := ts.URL + "/"; opened != want || !strings.Contains(out.String(), want) {
		t.Fatalf("opened %q, want %q\n%s", opened, want, out.String())
	}
}

func TestWebStartsServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	pages := make(chan string, 1)
	stubBrowser(t, func(url string) error {
		defer syscall.Kill(os.Getpid(), syscall.SIGINT)
		resp, err := http.Get(url)
		if err != nil {
			pages <- err.Error()
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		pages <- string(b)
		return nil
	})

	var out, errb bytes.Buffer
	args := []string{"web", "-port", port, "-root", t.TempDir(), "-jobs", "../../harbor/testdata/jobs", "-tasks", "../../harbor/testdata/tasks"}
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if page := <-pages; !strings.Contains(page, "<title>kavach bench</title>") {
		t.Fatalf("served page:\n%.300s", page)
	}
	if want := "http://127.0.0.1:" + port + "/"; !strings.Contains(out.String(), want) {
		t.Fatalf("no URL printed:\n%s", out.String())
	}
}

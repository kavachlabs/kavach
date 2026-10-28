package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h, err := Handler(Files)
	if err != nil {
		t.Fatal(err)
	}
	version, err := Version(Files)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/", "/index.html", "/app.js", "/app.css"} {
		w := get(path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control %q", path, got)
		}
		etag := w.Header().Get("ETag")
		if etag != `"`+version+`"` {
			t.Errorf("%s: ETag %q, want the version %q", path, etag, version)
		}
		if w := get(path, etag); w.Code != http.StatusNotModified {
			t.Errorf("%s with If-None-Match: status %d, want 304", path, w.Code)
		}
	}
	body := get("/", "").Body.String()
	if !strings.Contains(body, `href="app.css?v=`+version+`"`) {
		t.Errorf("index.html does not link app.css?v=%s", version)
	}
	if strings.Contains(body, `src="app.js?`) {
		t.Error("app.js is stamped; trial.js imports ./app.js, so it would run twice")
	}
}

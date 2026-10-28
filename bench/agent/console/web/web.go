// Package web is the console's web app: static HTML, CSS and JavaScript that
// the API server serves at /. There is no build step.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"time"
)

// Files are the app's files, served as they are.
//
//go:embed index.html app.css app.js invariants.js logos.js monaco.js replay.js trail.js trial.js uistate.js kavach.png kavach-icon.png kavach-mark.png fonts
var Files embed.FS

// Version is a short hash of every file in fsys, so it changes whenever any
// of them does.
func Version(fsys fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// Stamp returns index.html with its stylesheet at app.css?v=version, so a
// browser fetches the stylesheet again after a deploy. The modules keep
// their plain names: trial.js imports ./app.js, and app.js under a second
// URL would run twice.
func Stamp(index []byte, version string) []byte {
	return bytes.Replace(index, []byte(`href="app.css"`), []byte(`href="app.css?v=`+version+`"`), 1)
}

// Handler serves fsys with index.html stamped (Stamp), and every file with
// Cache-Control: no-cache and the app's version as its ETag: a browser asks
// again on each load and gets a 304 until a deploy changes a file, then the
// new files at once, the modules app.js imports among them.
func Handler(fsys fs.FS) (http.Handler, error) {
	version, err := Version(fsys)
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return nil, err
	}
	index = Stamp(index, version)
	files := http.FileServerFS(fsys)
	etag := `"` + version + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			// ServeContent answers If-None-Match from the ETag above.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
			return
		}
		files.ServeHTTP(w, r)
	}), nil
}

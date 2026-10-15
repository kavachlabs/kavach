// Package web is the console's web app: static HTML, CSS and JavaScript that
// the API server serves at /. There is no build step.
package web

import "embed"

// Files are the app's files, served as they are.
//
//go:embed index.html app.css app.js invariants.js logos.js monaco.js replay.js
var Files embed.FS

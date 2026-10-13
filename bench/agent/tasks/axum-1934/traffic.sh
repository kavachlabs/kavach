#!/usr/bin/env bash
# Prints the incident traffic, one request per line, for kavach-driver's
# standard input: API clients create, read and delete todos (one 404 and one
# rejected create among them); then a browser opens the front end, whose files
# the Router serves through its fallback; then the API is used again.
# Usage: traffic.sh | kavach-driver
set -euo pipefail

cat <<'EOF'
{"method":"GET","uri":"/api/todos"}
{"method":"POST","uri":"/api/todos","headers":{"content-type":"application/json"},"body":"{\"title\":\"buy milk\"}"}
{"method":"POST","uri":"/api/todos","headers":{"content-type":"application/json"},"body":"{\"title\":\"write report\"}"}
{"method":"GET","uri":"/api/todos/1"}
{"method":"POST","uri":"/api/todos","headers":{"content-type":"application/json"},"body":"{\"note\":\"no title\"}"}
{"method":"GET","uri":"/api/todos/7"}
{"method":"POST","uri":"/api/todos","headers":{"content-type":"application/json"},"body":"{\"title\":\"call the bank\"}"}
{"method":"DELETE","uri":"/api/todos/2"}
{"method":"GET","uri":"/api/todos"}
{"method":"GET","uri":"/","headers":{"accept":"text/html"}}
{"method":"GET","uri":"/style.css","headers":{"accept":"text/css"}}
{"method":"GET","uri":"/app.js","headers":{"accept":"*/*"}}
{"method":"GET","uri":"/api/todos"}
{"method":"GET","uri":"/favicon.ico","headers":{"accept":"image/*"}}
EOF

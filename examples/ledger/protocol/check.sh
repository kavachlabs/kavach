#!/bin/sh
# Records the buggy ledger through kavach-recorder, then checks the fixture
# with the kavach CLI: the buggy build reproduces the crash, the fixed build
# fixes it, and kavach diff accepts the fix. Needs Go and Julia.
#
#   examples/ledger/protocol/check.sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

(cd "$root" && go build -o "$work/kavach" ./cmd/kavach && go build -o "$work/kavach-recorder" ./cmd/kavach-recorder)
old="julia $here/ledger.jl"
new="julia $here/ledger.jl --fix"

# The buggy ledger crashes on purpose.
KAVACH_RECORDER=$work/kavach-recorder $old --in "$root/examples/ledger/testdata/events.jsonl" --fixtures "$work/journal" >/dev/null || true
set -- "$work"/journal/fixtures/*.kavach
[ $# -eq 1 ] && [ -f "$1" ] || { echo "check: expected one fixture, got: $*" >&2; exit 1; }
fixture=$1

expect() { # expect <want> <kavach args...>
	want=$1
	shift
	out=$("$work/kavach" "$@" 2>&1) || true
	case $out in
	*"$want"*) echo "ok    $want" ;;
	*) printf 'FAIL  want %s from kavach %s\n%s\n' "$want" "$*" "$out" >&2; exit 1 ;;
	esac
}
expect "result    still_failing@32" replay "$fixture" --bin "$old"
expect "result    fixed" replay "$fixture" --bin "$new"
expect '"verdict": "fixed"' diff --json "$fixture" --old "$old" --new "$new"

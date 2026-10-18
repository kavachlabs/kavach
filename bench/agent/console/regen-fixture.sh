#!/bin/sh
# Regenerates testdata/axum-1934.kavach: runs classifyhost, built at OLD_REV
# (the commit that added it, before the classifier was fixed), over
# testdata/axum-1934-inputs.jsonl, the error text of public trial job
# patient-lemon-ibex-but7lma with its Docker pull progress thinned. The run
# ends at the input that breaks the invariant and the recorder writes the fixture.
#
#   OLD_REV=<commit> bench/agent/console/regen-fixture.sh
set -eu
cd "$(dirname "$0")"
root=$(git rev-parse --show-toplevel)
rel=${PWD#"$root"/}
work=$(mktemp -d)
trap 'git -C "$root" worktree remove --force "$work/old" 2>/dev/null; rm -rf "$work"' EXIT

go build -o "$work/kavach-recorder" github.com/kavachlabs/kavach/cmd/kavach-recorder
git -C "$root" worktree add -q --detach "$work/old" "${OLD_REV:?set OLD_REV}"
(cd "$work/old/$rel" && go build -trimpath -o "$work/classifyhost" ./cmd/classifyhost)

KAVACH_RECORDER=$work/kavach-recorder "$work/classifyhost" -record testdata/axum-1934-inputs.jsonl -fixtures "$work/out" || true
set -- "$work"/out/fixtures/*.kavach
[ $# -eq 1 ] && [ -f "$1" ] || { echo "regen-fixture: expected one fixture, got: $*" >&2; exit 1; }
cp "$1" testdata/axum-1934.kavach

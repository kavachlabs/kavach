#!/bin/sh
# Regenerates ../testdata/null-amount.kavach: runs the buggy ledger over
# ../testdata/events.jsonl, recording through kavach-recorder with the neutral
# environment of ../testdata/null-amount-facts.json (kavach-recorder --test-facts),
# so that the fixture holds nothing of the machine it was made on.
#
#   examples/ledger/sdk/regen-fixture.sh
#
# The clock and random values in the fixture are the ones of this run, and
# host.runtime is the Go version that built the ledger. The ledger is built with
# -trimpath so that the panic stack in the fixture holds no local paths.
set -eu
cd "$(dirname "$0")"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

go build -o "$work/kavach-recorder" github.com/kavachlabs/kavach/cmd/kavach-recorder
go build -trimpath -o "$work/ledger" .
printf '#!/bin/sh\nexec "%s" --test-facts "%s" "$@"\n' "$work/kavach-recorder" "$PWD/../testdata/null-amount-facts.json" >"$work/recorder"
chmod +x "$work/recorder"

# The buggy ledger crashes on purpose.
KAVACH_RECORDER=$work/recorder "$work/ledger" -in ../testdata/events.jsonl -fixtures "$work/out" || true
set -- "$work"/out/fixtures/*.kavach
[ $# -eq 1 ] && [ -f "$1" ] || { echo "regen-fixture: expected one fixture, got: $*" >&2; exit 1; }
cp "$1" ../testdata/null-amount.kavach

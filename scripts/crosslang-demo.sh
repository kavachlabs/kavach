#!/usr/bin/env bash
# The cross-language demo: for every SDK in sdk/, record the ledger's crash
# with that SDK's recorder, reproduce it with `kavach replay` through the buggy
# build as a host command, and verify the fixed build with `kavach diff`.
#
#   scripts/crosslang-demo.sh [language...]     # default: all of them
#
# A language whose toolchain is missing is skipped. KEEP=1 keeps the work
# directory (builds, fixtures, logs) and prints where it is.
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
EVENTS=$ROOT/examples/ledger/testdata/events.jsonl
WORK=$(mktemp -d "${TMPDIR:-/tmp}/kavach-crosslang.XXXXXX")
if [ -z "${KEEP:-}" ]; then trap 'rm -rf "$WORK"' EXIT; else echo "work directory: $WORK" >&2; fi

LANGS=(python typescript rust c java php ruby elixir haskell ocaml julia)
[ $# -gt 0 ] && LANGS=("$@")

q() { printf '%q' "$1"; }
have() { command -v "$1" >/dev/null 2>&1; }
log() { echo "[$LANG_NAME] $*" >&2; }

(cd "$ROOT" && go build -o "$WORK/bin/kavach" ./cmd/kavach && go build -o "$WORK/bin/kavach-recorder" ./cmd/kavach-recorder) || { echo "cannot build kavach" >&2; exit 1; }
KAVACH=$WORK/bin/kavach
export KAVACH_RECORDER=$WORK/bin/kavach-recorder
export KAVACH_NO_BANNER=1 NO_COLOR=1

# Each setup_<language> checks for its toolchain, builds the ledger into $WORK
# and sets DIR (where to run), RECORD (a shell command recording the buggy
# build's crash into $FX), BUGGY and FIXED (host commands). It returns 1, with
# NOTE set, if the language cannot be run.
setup_python() {
  have python3 || { NOTE="no python3"; return 1; }
  DIR=$ROOT/sdk/python/examples/ledger
  export PYTHONPATH=$ROOT/sdk/python
  BUGGY="python3 -m ledger"; FIXED="python3 -m ledger --fix"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_typescript() {
  have node && have npm || { NOTE="no node/npm"; return 1; }
  cp -R "$ROOT/sdk/typescript" "$WORK/ts"
  (cd "$WORK/ts" && npm ci --silent && npm run build --silent) >"$WORK/ts-build.log" 2>&1 || { NOTE="build failed ($WORK/ts-build.log)"; return 1; }
  DIR=$WORK/ts
  BUGGY="node $(q "$WORK/ts/dist/examples/ledger/main.js")"; FIXED="$BUGGY --fixed"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_rust() {
  have cargo || { NOTE="no cargo"; return 1; }
  cargo build --quiet --release --example ledger --manifest-path "$ROOT/sdk/rust/Cargo.toml" --target-dir "$WORK/rust" >"$WORK/rust-build.log" 2>&1 || { NOTE="build failed ($WORK/rust-build.log)"; return 1; }
  DIR=$WORK
  BUGGY=$(q "$WORK/rust/release/examples/ledger"); FIXED="$BUGGY --fixed"
  RECORD="$BUGGY --in $(q "$EVENTS") --dir $(q "$FX")"
}

setup_c() {
  have cmake && have clang && have clang++ || { NOTE="no cmake/clang"; return 1; }
  { cmake -S "$ROOT/sdk/c" -B "$WORK/c" -DCMAKE_C_COMPILER=clang -DCMAKE_CXX_COMPILER=clang++ -DKAVACH_BUILD_TESTS=OFF -DKAVACH_WERROR=OFF &&
    cmake --build "$WORK/c" --target ledger-buggy ledger-fixed; } >"$WORK/c-build.log" 2>&1 || { NOTE="build failed ($WORK/c-build.log)"; return 1; }
  DIR=$WORK
  BUGGY=$(q "$WORK/c/examples/ledger/ledger-buggy"); FIXED=$(q "$WORK/c/examples/ledger/ledger-fixed")
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_java() {
  have javac && have jar && have java || { NOTE="no JDK"; return 1; }
  local out=$WORK/java
  mkdir -p "$out/classes" "$out/examples"
  { javac --release 21 -encoding UTF-8 -d "$out/classes" $(find "$ROOT/sdk/java/src/main" -name '*.java') &&
    javac --release 21 -encoding UTF-8 -cp "$out/classes" -d "$out/examples" $(find "$ROOT/sdk/java/examples" -name '*.java'); } >"$WORK/java-build.log" 2>&1 || { NOTE="build failed ($WORK/java-build.log)"; return 1; }
  DIR=$WORK
  BUGGY="java -cp $(q "$out/classes:$out/examples") ledger.Main"; FIXED="$BUGGY --fix"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_php() {
  have php || { NOTE="no php"; return 1; }
  DIR=$WORK
  BUGGY="php $(q "$ROOT/sdk/php/examples/ledger/main.php")"; FIXED="$BUGGY --fix"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_ruby() {
  local ruby=ruby
  [ -x /opt/homebrew/opt/ruby/bin/ruby ] && ruby=/opt/homebrew/opt/ruby/bin/ruby
  have "$ruby" || { NOTE="no ruby"; return 1; }
  "$ruby" -e 'exit(RUBY_VERSION >= "3.1" ? 0 : 1)' || { NOTE="ruby too old"; return 1; }
  DIR=$WORK
  BUGGY="$(q "$ruby") -I$(q "$ROOT/sdk/ruby/lib") $(q "$ROOT/sdk/ruby/examples/ledger/ledger.rb")"; FIXED="$BUGGY --fix"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_elixir() {
  have mix && have elixir || { NOTE="no elixir"; return 1; }
  cp -R "$ROOT/sdk/elixir" "$WORK/elixir"
  (cd "$WORK/elixir/examples/ledger" && MIX_ENV=prod mix escript.build) >"$WORK/elixir-build.log" 2>&1 || { NOTE="build failed ($WORK/elixir-build.log)"; return 1; }
  DIR=$WORK
  BUGGY=$(q "$WORK/elixir/examples/ledger/ledger"); FIXED="$BUGGY --fix"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_haskell() {
  have cabal && have ghc || { NOTE="no cabal/ghc"; return 1; }
  (cd "$ROOT/sdk/haskell" && cabal build --builddir="$WORK/hs" exe:ledger) >"$WORK/hs-build.log" 2>&1 || { NOTE="build failed ($WORK/hs-build.log)"; return 1; }
  local bin
  bin=$(cd "$ROOT/sdk/haskell" && cabal list-bin --builddir="$WORK/hs" exe:ledger) || { NOTE="no ledger binary"; return 1; }
  DIR=$WORK
  BUGGY=$(q "$bin"); FIXED="$BUGGY --fix"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_ocaml() {
  have dune && have ocaml || { NOTE="no dune/ocaml"; return 1; }
  dune build --root "$ROOT/sdk/ocaml" --build-dir "$WORK/ocaml" ./examples/ledger/ledger.exe >"$WORK/ocaml-build.log" 2>&1 || { NOTE="build failed ($WORK/ocaml-build.log)"; return 1; }
  DIR=$WORK
  BUGGY=$(q "$WORK/ocaml/default/examples/ledger/ledger.exe"); FIXED="$BUGGY --fix"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

setup_julia() {
  have julia || { NOTE="no julia"; return 1; }
  DIR=$WORK
  BUGGY="julia --project=$(q "$ROOT/sdk/julia") $(q "$ROOT/sdk/julia/examples/ledger/ledger.jl")"; FIXED="$BUGGY --fix"
  RECORD="$BUGGY --in $(q "$EVENTS") --fixtures $(q "$FX")"
}

# field NAME FILE [INDENT]: the value of a key of kavach's --json output, a
# top-level one (the default) or one at INDENT spaces (the variants object).
field() { sed -n "s/^ \{${3:-2}\}\"$1\": \"\{0,1\}\(.*[^\",]\)\"\{0,1\},\{0,1\}$/\1/p" "$2" | head -1 | sed 's/\\"/"/g'; }

ROWS=()
row() { ROWS+=("$(printf '%-11s %-18s %-24s %-16s %-6s %s' "$@")"); }

for LANG_NAME in "${LANGS[@]}"; do
  FX=$WORK/fx-$LANG_NAME
  mkdir -p "$FX"
  NOTE=""
  if ! (declare -F "setup_$LANG_NAME" >/dev/null); then row "$LANG_NAME" - - - - "unknown language"; continue; fi
  # A subshell keeps one language's variables and environment from the next.
  out=$(
    "setup_$LANG_NAME" || { echo "skip|$NOTE"; exit 0; }
    cd "$DIR" || exit 1
    log "recording"
    (eval "$RECORD") >"$WORK/$LANG_NAME-record.log" 2>&1
    fixture=$(ls "$FX"/fixtures/*.kavach 2>/dev/null | head -1)
    [ -n "$fixture" ] || { echo "fail|no fixture written ($WORK/$LANG_NAME-record.log)"; exit 0; }
    log "replaying $(basename "$fixture")"
    "$KAVACH" replay "$fixture" --bin "$BUGGY" --json >"$WORK/$LANG_NAME-replay.json" 2>"$WORK/$LANG_NAME-replay.err"
    replay=$(field verdict "$WORK/$LANG_NAME-replay.json")
    drift=$(grep -c '"recorded":' "$WORK/$LANG_NAME-replay.json")
    log "verifying the fix"
    "$KAVACH" diff "$fixture" --old "$BUGGY" --new "$FIXED" --json >"$WORK/$LANG_NAME-diff.json" 2>"$WORK/$LANG_NAME-diff.err"
    verdict=$(field verdict "$WORK/$LANG_NAME-diff.json")
    repro=$(field reproducing "$WORK/$LANG_NAME-diff.json" 4); cand=$(field candidates "$WORK/$LANG_NAME-diff.json" 4); pass=$(field passing "$WORK/$LANG_NAME-diff.json" 4)
    detail=$(field detail "$WORK/$LANG_NAME-diff.json")
    [ -n "$replay" ] || replay="error: $(head -c 150 "$WORK/$LANG_NAME-replay.err" | tr '\n' ' ')"
    [ -n "$verdict" ] || verdict="error: $(head -c 150 "$WORK/$LANG_NAME-diff.err" | tr '\n' ' ')"
    echo "ok|$replay|$verdict|$repro/$cand ($pass pass)|$drift|$detail"
  )
  IFS='|' read -r state a b c d e <<<"$out"
  case "$state" in
    ok) row "$LANG_NAME" "$a" "$b" "$c" "$d" "$e" ;;
    skip) row "$LANG_NAME" - - - - "skipped: $a" ;;
    *) row "$LANG_NAME" - - - - "${a:-failed}" ;;
  esac
done

echo
printf '%-11s %-18s %-24s %-16s %-6s %s\n' language replay diff variants drift note
printf '%s\n' "${ROWS[@]}"

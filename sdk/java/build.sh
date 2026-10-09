#!/usr/bin/env bash
# Builds the Kavach Java SDK with the JDK alone (no Maven, no Gradle).
#
#   ./build.sh          compile the SDK, the conformance mains and the examples
#   ./build.sh test     build, run the unit tests, the 7 SDK recorder cases and
#                       the 18 host transcripts
#   ./build.sh clean    remove build/
#
# KAVACH_SPEC_DIR points at the repository's spec/ directory (default: ../../spec).
set -euo pipefail
cd "$(dirname "$0")"

SPEC_DIR="${KAVACH_SPEC_DIR:-$(cd ../.. && pwd)/spec}"
export KAVACH_SPEC_DIR="$SPEC_DIR"
OUT=build
CLASSES=$OUT/classes
JAVAC=(javac --release 21 -Xlint:all,-serial -encoding UTF-8)

build() {
  rm -rf "$OUT"
  mkdir -p "$CLASSES" "$OUT/test-classes" "$OUT/examples"
  "${JAVAC[@]}" -d "$CLASSES" $(find src/main -name '*.java')
  jar --create --file "$OUT/kavach.jar" -C "$CLASSES" .
  "${JAVAC[@]}" -cp "$CLASSES" -d "$OUT/examples" $(find examples -name '*.java')
  echo "built $OUT/kavach.jar and $OUT/examples"
}

run_tests() {
  "${JAVAC[@]}" -cp "$CLASSES" -d "$OUT/test-classes" $(find src/test -name '*.java')
  java -cp "$CLASSES:$OUT/test-classes" com.kavachlabs.kavach.AllTests

  [ -d "$SPEC_DIR/recorder/sdk" ] || { echo "no $SPEC_DIR/recorder/sdk: set KAVACH_SPEC_DIR" >&2; exit 1; }
  java -cp "$CLASSES" com.kavachlabs.kavach.conformance.RecorderCase "$SPEC_DIR"/recorder/sdk/*.json

  python3 "$SPEC_DIR/host/run.py" --host "java -cp '$PWD/$CLASSES' com.kavachlabs.kavach.conformance.ConformanceHost"
}

case "${1:-build}" in
  build) build ;;
  test) build; run_tests ;;
  clean) rm -rf "$OUT" ;;
  *) echo "usage: $0 [build|test|clean]" >&2; exit 2 ;;
esac

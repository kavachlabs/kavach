#!/usr/bin/env bash
# Records incident.kavach in the Kavach arm's image, so the fixture's
# environment facts are the container's, not a developer machine's. From the
# repository root, after building the kavach-build target:
#
#   docker run --rm --network none -v "$PWD/bench/agent/tasks/axum-1934:/t:ro" \
#     -v "$PWD/bench/agent/results/axum-1934-rec:/rec" \
#     kavach-bench/axum-1934:kavach-build /t/record.sh
#
# writes /rec/incident.kavach (the fixture of the first failure) and replays
# it on the old build.
set -euo pipefail
mkdir -p /tmp/rec && cd /tmp/rec
# The recorder captures the process's environment (SPEC 4.8): keep it minimal.
/t/traffic.sh | env -i PATH=/opt/kavach/bin:/usr/bin:/bin HOME=/nonexistent LANG=C TZ=UTC \
  KAVACH_DIR=journal /work/target/debug/kavach-driver >driver.out 2>driver.err
cp "$(ls journal/fixtures/*.kavach | sort | head -1)" /rec/incident.kavach
kavach --no-banner replay /rec/incident.kavach --bin /opt/axum-old/kavach-driver || true

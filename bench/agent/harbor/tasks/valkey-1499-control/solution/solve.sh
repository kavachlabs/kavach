#!/bin/bash
# The upstream fix (candidates/upstream.patch).
set -euo pipefail
cd /work
git apply --allow-empty /solution/fix.patch

#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SDK_ROOT="$(cd "${ROOT}/../.." && pwd)"
REPO_ROOT="$(cd "${SDK_ROOT}/../.." && pwd)"
ENTERPRISE_INC="/Users/koustav/code/kavach-labs/kavach-enterprise/include"

export KAVACH_NO_BANNER=1
export KAVACH_NO_OPEN=1

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kavach-ts-e2e-XXXXXX")"
trap 'rm -rf "${TMP_DIR}"' EXIT

if [[ -z "${KAVACH:-}" ]]; then
  KAVACH="${TMP_DIR}/kavach"
  (cd "${REPO_ROOT}" && GOWORK=off go build -o "${KAVACH}" ./cmd/kavach)
fi

echo "==> 1. Build, typecheck, and run all TypeScript unit/integration tests"
(cd "${SDK_ROOT}" && npm run build && npm run typecheck && npm test)

echo "==> 2. Verify C++23 kavach::daemon::ShmRingConsumer decodes TS-produced KVRING02 ring"
TS_RING="${TMP_DIR}/ts_producer.shm"
node --input-type=module - "${TS_RING}" "${SDK_ROOT}/packages/core/dist/index.js" <<'EOF'
import { init } from "@kavachlabs/core";
const ringPath = process.argv[2];
const kv = init({ service: "ts-cpp-interop", ringPath, pii: "standard", mode: "record" });
await kv.runStep(
  { source: "kafka:events_raw", position: "0:1", data: Buffer.from('{"transaction_id":"tx_cpp_1","email":"c.user@vertex-ai.example"}') },
  async () => {
    kv.now();
    kv.rand().bytes(8);
    kv.recordGateway({
      driver: "pg",
      gateway: "pg:select",
      op: "SELECT id FROM subscriptions WHERE id = $1",
      keyArgs: ["sub_1"],
      response: Buffer.from('{"columns":["id"],"rows":[[{"t":"string","v":"sub_1"}]]}'),
    });
    kv.recordOutput({
      sink: "kafka:events_charged_in_advance",
      data: Buffer.from('{"transaction_id":"tx_cpp_1","email":"c.user@vertex-ai.example"}'),
    });
  },
);
await kv.close();
EOF

cat > "${TMP_DIR}/verify_ring.cpp" <<'CPP'
#include <iostream>
#include <string>
#include "kavach/daemon/shm_consumer.hpp"

int main(int argc, char** argv) {
  if (argc < 2) return 2;
  auto consumer = kavach::daemon::ShmRingConsumer::open_path(argv[1]);
  if (!consumer) {
    std::cerr << "open_path failed: " << consumer.error() << "\n";
    return 1;
  }
  auto res = consumer->finish_on_eof();
  if (!res) {
    std::cerr << "finish_on_eof failed: " << res.error() << "\n";
    return 1;
  }
  std::cout << "CPP_SHM_RING_CONSUMER frames=" << res->frames.size()
            << " clean_close=" << (res->clean_close ? "true" : "false")
            << " torn=" << res->discarded_torn_bytes
            << " synth_marker=" << (res->synthesized_marker.has_value() ? "true" : "false")
            << "\n";
  if (!res->clean_close || res->discarded_torn_bytes != 0 || res->synthesized_marker.has_value()) {
    return 1;
  }
  if (res->frames.size() != 8) {
    std::cerr << "expected 8 frames (Open+Input+Clock+Rand+Gateway+Output+StepEnd+Close), got "
              << res->frames.size() << "\n";
    return 1;
  }
  return 0;
}
CPP

CXX="${CXX:-/usr/bin/clang++}"
if [[ ! -x "${CXX}" ]]; then
  CXX="clang++"
fi
SYSROOT_FLAGS=()
if command -v xcrun >/dev/null 2>&1; then
  SDK_PATH="$(xcrun --show-sdk-path 2>/dev/null || true)"
  if [[ -n "${SDK_PATH}" ]]; then
    SYSROOT_FLAGS=(-isysroot "${SDK_PATH}")
  fi
fi
"${CXX}" -std=c++23 "${SYSROOT_FLAGS[@]}" -I"${ENTERPRISE_INC}" "${TMP_DIR}/verify_ring.cpp" -o "${TMP_DIR}/verify_ring"
"${TMP_DIR}/verify_ring" "${TS_RING}"

echo "==> 3. Record PII-scrubbed baseline capsule via kavach dev"
CAPSULES_DIR="${TMP_DIR}/capsules"
mkdir -p "${CAPSULES_DIR}"
(cd "${ROOT}" && "${KAVACH}" dev --cmd "node ${ROOT}/record.ts" --out "${CAPSULES_DIR}" --service l1-billing-ts --pii standard --json > "${TMP_DIR}/dev_result.json")
cat "${TMP_DIR}/dev_result.json"
CAPSULE="$(ls "${CAPSULES_DIR}/"*.kavach | head -n 1)"
if [[ ! -f "${CAPSULE}" ]]; then
  echo "ERROR: kavach dev did not produce a capsule!" >&2
  exit 1
fi

echo "==> 4. Verify zero raw PII leaked into capsule"
if "${KAVACH}" inspect --full "${CAPSULE}" | grep -q "vertex-ai.example"; then
  echo "ERROR: raw PII email found in recorded capsule!" >&2
  exit 1
fi
echo "PII audit PASS (0 raw emails); scrubbed deterministic tokens:"
"${KAVACH}" inspect --full "${CAPSULE}" | grep -oE 'u_[0-9a-f]{16}@redacted\.invalid' | sort -u

echo "==> 5. Replay baseline capsule on old.ts and new.ts (100% offline)"
(cd "${ROOT}" && "${KAVACH}" replay "${CAPSULE}" --bin "node ${ROOT}/old.ts" --invariants "${ROOT}/.kavach/invariants.yaml" --json > "${TMP_DIR}/replay_old.json")
(cd "${ROOT}" && "${KAVACH}" replay "${CAPSULE}" --bin "node ${ROOT}/new.ts" --invariants "${ROOT}/.kavach/invariants.yaml" --json > "${TMP_DIR}/replay_new.json")

echo "==> 6. Run kavach diff --variants 25 (old vs new -> verdict: fixed)"
(cd "${ROOT}" && "${KAVACH}" diff "${CAPSULE}" \
  --old "node ${ROOT}/old.ts" \
  --new "node ${ROOT}/new.ts" \
  --invariants "${ROOT}/.kavach/invariants.yaml" \
  --variants 25 \
  --json > "${TMP_DIR}/diff_fixed.json")

echo "==> 7. Run kavach diff --variants 25 (old vs old -> must fail atomic_single_emit_on_retry)"
set +e
(cd "${ROOT}" && "${KAVACH}" diff "${CAPSULE}" \
  --old "node ${ROOT}/old.ts" \
  --new "node ${ROOT}/old.ts" \
  --invariants "${ROOT}/.kavach/invariants.yaml" \
  --variants 25 \
  --json > "${TMP_DIR}/diff_unfixed.json")
UNFIXED_EXIT=$?
set -e

python3 - "${TMP_DIR}/replay_old.json" "${TMP_DIR}/replay_new.json" "${TMP_DIR}/diff_fixed.json" "${TMP_DIR}/diff_unfixed.json" "${UNFIXED_EXIT}" <<'PY'
import json, sys
ro, rn, df, du = [json.load(open(p)) for p in sys.argv[1:5]]
unfixed_exit = int(sys.argv[5])
print(f"replay(old): {ro.get('verdict')} ({ro.get('wall_ms')} ms)")
print(f"replay(new): {rn.get('verdict')} ({rn.get('wall_ms')} ms)")
v = df.get("variants", {})
r = df.get("regressions", {})
print(f"diff(old vs new): verdict={df.get('verdict')} wall_ms={df.get('wall_ms')} reproducing={v.get('reproducing')} passing={v.get('passing')} regressions={r.get('passing')}/{r.get('counted')}")
print(f"diff(old vs old): verdict={du.get('verdict')} exit={unfixed_exit} detail={du.get('detail')}")

assert ro.get("verdict") == "ok", f"expected replay(old)=ok, got {ro}"
assert rn.get("verdict") == "ok", f"expected replay(new)=ok, got {rn}"
assert df.get("verdict") == "fixed", f"expected diff(old vs new)=fixed, got {df.get('verdict')}: {df.get('detail')}"
assert v.get("reproducing", 0) >= 25, f"expected >=25 reproducing variants, got {v}"
assert v.get("passing") == v.get("reproducing"), f"not all reproducing variants passed: {v}"
assert r.get("passing") == r.get("counted"), f"regression detected: {r}"
assert unfixed_exit != 0, "expected old vs old diff to exit non-zero"
assert du.get("verdict") != "fixed", f"expected old vs old NOT fixed, got {du.get('verdict')}"
# Confirm atomic_single_emit_on_retry was caught on old vs old
du_text = json.dumps(du)
assert "atomic_single_emit_on_retry" in du_text, f"expected atomic_single_emit_on_retry in old vs old failure: {du_text[:500]}"
print("ALL E2E ASSERTIONS PASSED!")
PY

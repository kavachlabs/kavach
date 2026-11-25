#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${ROOT}/../.." && pwd)"
PACKAGES=(core http pg ioredis kafkajs)

DRY_RUN=0
for arg in "$@"; do
  case "${arg}" in
    --dry-run)
      DRY_RUN=1
      ;;
    -h|--help)
      echo "Usage: $(basename "$0") [--dry-run]"
      echo "Builds, typechecks, tests, and publishes @kavachlabs/{core,http,pg,ioredis,kafkajs} to npm."
      exit 0
      ;;
    *)
      echo "publish.sh: unknown argument: ${arg}" >&2
      exit 2
      ;;
  esac
done

echo "==> Checking npm authentication"
if npm whoami >/dev/null 2>&1; then
  echo "Authenticated as npm user: $(npm whoami)"
elif [[ "${DRY_RUN}" -eq 1 ]]; then
  echo "Dry-run mode: not logged into npm (proceeding with local --dry-run verification)"
else
  echo "ERROR: not authenticated to npm. Run 'npm login' or set NPM_TOKEN before publishing." >&2
  exit 1
fi

echo "==> Building, typechecking, and running full test suite"
(cd "${ROOT}" && npm run build && npm run typecheck && npm test)

# Stage LICENSE into each package during publish so every npm tarball carries Apache-2.0 LICENSE.
COPIED_LICENSES=()
cleanup() {
  for f in "${COPIED_LICENSES[@]}"; do
    rm -f "${f}"
  done
}
trap cleanup EXIT

ROOT_LICENSE="${REPO_ROOT}/LICENSE"
for pkg in "${PACKAGES[@]}"; do
  pkg_dir="${ROOT}/packages/${pkg}"
  if [[ -f "${ROOT_LICENSE}" && ! -f "${pkg_dir}/LICENSE" ]]; then
    cp "${ROOT_LICENSE}" "${pkg_dir}/LICENSE"
    COPIED_LICENSES+=("${pkg_dir}/LICENSE")
  fi
done

PUBLISH_FLAGS=(--access public)
if [[ "${DRY_RUN}" -eq 1 ]]; then
  PUBLISH_FLAGS+=(--dry-run)
fi

for pkg in "${PACKAGES[@]}"; do
  pkg_dir="${ROOT}/packages/${pkg}"
  echo "==> Publishing @kavachlabs/${pkg} (${PUBLISH_FLAGS[*]})"
  (cd "${pkg_dir}" && npm publish "${PUBLISH_FLAGS[@]}")
done

echo "==> Publish complete (dry_run=${DRY_RUN})"

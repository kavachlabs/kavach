#!/bin/sh
# Installs kavach, kavach-recorder, and the Kavach Agent Skills on Linux or macOS (amd64 / arm64).
#
# Usage:
#   curl -fsSL https://www.thekavachlabs.com/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/kavachlabs/kavach/main/scripts/install.sh | sh
#
# Flags:
#   --skills-only       Install only the agent skills (kavach-fix, kavach-implement, kavach-setup, kavach-invariants, kavach-verify)
#   --no-skills         Install only the kavach and kavach-recorder binaries
#   --version <tag>     Release tag to install, e.g. v0.2.1 (default: latest release)
#   -h, --help          Show this help message
#
# Environment variables:
#   KAVACH_INSTALL_DIR  Binary destination directory (default: /usr/local/bin if writable, else ~/.local/bin)
#   KAVACH_SKILLS_DIR   Explicit skills destination directory (default: ~/.claude/skills, plus ~/.cursor/skills,
#                       ~/.codex/skills, ~/.config/opencode/skills and ~/.gemini/config/skills (Antigravity) if present)
#   KAVACH_SKIP_SKILLS  If set to 1, skip installing agent skills
#   KAVACH_VERSION      Release tag to install, e.g. v0.2.1 (default: latest release)
#   KAVACH_FROM_SOURCE  If set to 1, install binaries via 'go install' instead of downloading release archives
#   KAVACH_ARCHIVE_DIR  Local directory containing release tarballs and SHA256SUMS (for offline/CI tests)

set -eu

REPO="kavachlabs/kavach"
SKILLS_ONLY=0
SKIP_SKILLS="${KAVACH_SKIP_SKILLS:-0}"
VERSION_ARG="${KAVACH_VERSION:-}"
TMP_DIR=""

usage() {
  cat <<'EOF'
Usage: install.sh [OPTIONS]

Installs kavach, kavach-recorder, and the Kavach Agent Skills on Linux or macOS (amd64 / arm64).

Options:
  --skills-only       Install only the agent skills
  --no-skills         Install only the kavach and kavach-recorder binaries
  --version <tag>     Release tag to install, e.g. v0.2.1 (default: latest release)
  -h, --help          Show this help message

Environment variables:
  KAVACH_INSTALL_DIR  Binary destination directory (default: /usr/local/bin if writable, else ~/.local/bin)
  KAVACH_SKILLS_DIR   Explicit skills destination directory
  KAVACH_SKIP_SKILLS  If set to 1, skip installing agent skills
  KAVACH_VERSION      Release tag to install, e.g. v0.2.1 (default: latest release)
  KAVACH_FROM_SOURCE  If set to 1, install binaries via 'go install'
  KAVACH_ARCHIVE_DIR  Local directory containing release tarballs and SHA256SUMS
EOF
}

say() {
  printf 'kavach-install: %s\n' "$*" >&2
}

err() {
  say "error: $*"
  exit 1
}

cleanup() {
  if [ -n "${TMP_DIR:-}" ] && [ -d "$TMP_DIR" ]; then
    rm -rf "$TMP_DIR"
  fi
}

parse_args() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --skills-only)
        SKILLS_ONLY=1
        ;;
      --no-skills)
        SKIP_SKILLS=1
        ;;
      --version)
        shift
        [ "$#" -gt 0 ] || err "--version requires a value (e.g. --version v0.2.1)"
        VERSION_ARG="$1"
        ;;
      --version=*)
        VERSION_ARG="${1#--version=}"
        [ -n "$VERSION_ARG" ] || err "--version requires a non-empty value"
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        err "unknown argument: $1 (run with --help for usage)"
        ;;
    esac
    shift
  done
}

normalize_tag() {
  raw="$1"
  if [ -z "$raw" ]; then
    printf '\n'
    return
  fi
  case "$raw" in
    v*) printf '%s\n' "$raw" ;;
    *)  printf 'v%s\n' "$raw" ;;
  esac
}

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    *)       err "unsupported operating system: $(uname -s) (use 'go install github.com/${REPO}/cmd/kavach@latest')" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)             err "unsupported architecture: $(uname -m) (use 'go install github.com/${REPO}/cmd/kavach@latest')" ;;
  esac
}

choose_install_dir() {
  if [ -n "${KAVACH_INSTALL_DIR:-}" ]; then
    echo "$KAVACH_INSTALL_DIR"
    return
  fi
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    echo "/usr/local/bin"
    return
  fi
  echo "${HOME}/.local/bin"
}

fetch() {
  url="$1"
  out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
  else
    err "either curl or wget is required"
  fi
}

resolve_latest_tag() {
  tmp="$1"
  api_url="https://api.github.com/repos/${REPO}/releases/latest"
  tag=""
  if fetch "$api_url" "${tmp}/release.json" 2>/dev/null; then
    tag="$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "${tmp}/release.json" | head -n 1)"
  fi
  # Fallback to GitHub's non-API redirect if api.github.com is rate-limited.
  if [ -z "$tag" ] && command -v curl >/dev/null 2>&1; then
    eff_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
    case "$eff_url" in
      */releases/tag/*)
        tag="${eff_url##*/releases/tag/}"
        tag="${tag%%\?*}"
        tag="${tag%/}"
        ;;
    esac
  fi
  printf '%s\n' "$tag"
}

verify_sha256() {
  dir="$1"
  file="$2"
  sums="$3"
  expected="$(awk -v f="$file" '{ sub(/\r$/, ""); if ($2 == f || $2 == "*"f || $2 == "./"f) { print $1; exit } }' "$sums")"
  if [ -z "$expected" ]; then
    err "checksum for ${file} not found in SHA256SUMS"
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "${dir}/${file}" | awk '{ print $1 }')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "${dir}/${file}" | awk '{ print $1 }')"
  elif command -v openssl >/dev/null 2>&1; then
    actual="$(openssl dgst -sha256 "${dir}/${file}" | awk '{ print $NF }')"
  else
    say "warning: no sha256 utility found; skipping checksum check"
    return 0
  fi
  if [ "$actual" != "$expected" ]; then
    err "SHA-256 mismatch for ${file}: expected ${expected}, got ${actual}"
  fi
}

install_from_source() {
  dest="$1"
  if ! command -v go >/dev/null 2>&1; then
    err "prebuilt release not found and 'go' is not installed"
  fi
  ref="latest"
  if [ -n "$VERSION_ARG" ]; then
    ref="$(normalize_tag "$VERSION_ARG")"
  fi
  say "building kavach and kavach-recorder from source (${ref}) via go install..."
  mkdir -p "$dest"
  GOBIN="$dest" go install "github.com/${REPO}/cmd/kavach@${ref}"
  GOBIN="$dest" go install "github.com/${REPO}/cmd/kavach-recorder@${ref}"
}

copy_skills_to() {
  src_skills="$1"
  target_dir="$2"
  mkdir -p "$target_dir"
  installed_list=""
  for skill_path in "${src_skills}"/kavach-*; do
    if [ -d "$skill_path" ] && [ -f "${skill_path}/SKILL.md" ]; then
      skill_name="$(basename "$skill_path")"
      rm -rf "${target_dir}/${skill_name}"
      cp -R "$skill_path" "${target_dir}/${skill_name}"
      installed_list="${installed_list:+${installed_list}, }${skill_name}"
    fi
  done
  if [ -n "$installed_list" ]; then
    say "installed agent skills (${installed_list}) to ${target_dir}"
  fi
}

install_skills() {
  tmp="$1"
  src_skills="${tmp}/skills"

  if [ ! -d "$src_skills" ]; then
    say "fetching agent skills from ${REPO}..."
    fetched_repo=0
    if [ -n "$VERSION_ARG" ]; then
      ref_tag="$(normalize_tag "$VERSION_ARG")"
      if fetch "https://codeload.github.com/${REPO}/tar.gz/refs/tags/${ref_tag}" "${tmp}/repo.tar.gz" 2>/dev/null; then
        fetched_repo=1
      fi
    fi
    if [ "$fetched_repo" = "0" ]; then
      if fetch "https://codeload.github.com/${REPO}/tar.gz/refs/heads/main" "${tmp}/repo.tar.gz"; then
        fetched_repo=1
      fi
    fi
    if [ "$fetched_repo" = "1" ]; then
      mkdir -p "${tmp}/repo"
      tar -xzf "${tmp}/repo.tar.gz" -C "${tmp}/repo" --strip-components=1
      src_skills="${tmp}/repo/skills"
    fi
  fi

  if [ ! -d "$src_skills" ]; then
    say "warning: agent skills directory not found; skipping skills install"
    return 0
  fi

  if [ -n "${KAVACH_SKILLS_DIR:-}" ]; then
    copy_skills_to "$src_skills" "$KAVACH_SKILLS_DIR"
    return 0
  fi

  installed_any=0
  claude_home="${CLAUDE_CONFIG_DIR:-${HOME}/.claude}"
  cursor_home="${HOME}/.cursor"
  codex_home="${CODEX_HOME:-${HOME}/.codex}"
  opencode_home="${HOME}/.config/opencode"
  antigravity_home="${HOME}/.gemini/config"

  if [ -d "$claude_home" ] || command -v claude >/dev/null 2>&1; then
    copy_skills_to "$src_skills" "${claude_home}/skills"
    installed_any=1
  fi

  if [ -d "$cursor_home" ]; then
    copy_skills_to "$src_skills" "${cursor_home}/skills"
    installed_any=1
  fi

  if [ -d "$codex_home" ] || command -v codex >/dev/null 2>&1; then
    copy_skills_to "$src_skills" "${codex_home}/skills"
    installed_any=1
  fi

  if [ -d "$opencode_home" ] || command -v opencode >/dev/null 2>&1; then
    copy_skills_to "$src_skills" "${opencode_home}/skills"
    installed_any=1
  fi

  if [ -d "$antigravity_home" ]; then
    copy_skills_to "$src_skills" "${antigravity_home}/skills"
    installed_any=1
  fi

  if [ "$installed_any" = "0" ]; then
    copy_skills_to "$src_skills" "${claude_home}/skills"
  fi
}

main() {
  parse_args "$@"

  TMP_DIR="$(mktemp -d)"
  trap cleanup EXIT
  trap 'cleanup; exit 130' INT
  trap 'cleanup; exit 143' TERM

  if [ "$SKILLS_ONLY" = "1" ]; then
    install_skills "$TMP_DIR"
    return 0
  fi

  dest="$(choose_install_dir)"
  mkdir -p "$dest"

  if [ "${KAVACH_FROM_SOURCE:-0}" = "1" ]; then
    install_from_source "$dest"
  else
    os="$(detect_os)"
    arch="$(detect_arch)"

    if [ -n "${KAVACH_ARCHIVE_DIR:-}" ]; then
      tag="$(normalize_tag "${VERSION_ARG:-v0.1.0-dev}")"
      ver="${tag#v}"
      tarball="kavach_${ver}_${os}_${arch}.tar.gz"
      [ -f "${KAVACH_ARCHIVE_DIR}/${tarball}" ] || err "archive not found: ${KAVACH_ARCHIVE_DIR}/${tarball}"
      [ -f "${KAVACH_ARCHIVE_DIR}/SHA256SUMS" ] || err "checksums file not found: ${KAVACH_ARCHIVE_DIR}/SHA256SUMS"
      cp "${KAVACH_ARCHIVE_DIR}/${tarball}" "${TMP_DIR}/${tarball}"
      cp "${KAVACH_ARCHIVE_DIR}/SHA256SUMS" "${TMP_DIR}/SHA256SUMS"
      verify_sha256 "$TMP_DIR" "$tarball" "${TMP_DIR}/SHA256SUMS"
      tar -xzf "${TMP_DIR}/${tarball}" -C "$TMP_DIR"
      install -m 0755 "${TMP_DIR}/kavach" "${dest}/kavach"
      install -m 0755 "${TMP_DIR}/kavach-recorder" "${dest}/kavach-recorder"
    else
      tag="$(normalize_tag "$VERSION_ARG")"
      if [ -z "$tag" ]; then
        tag="$(resolve_latest_tag "$TMP_DIR")"
      fi

      if [ -z "$tag" ]; then
        say "no prebuilt release tag found yet; falling back to go install"
        install_from_source "$dest"
      else
        ver="${tag#v}"
        tarball="kavach_${ver}_${os}_${arch}.tar.gz"
        base_url="https://github.com/${REPO}/releases/download/${tag}"
        say "downloading ${tarball} (${tag})..."
        if fetch "${base_url}/${tarball}" "${TMP_DIR}/${tarball}" && fetch "${base_url}/SHA256SUMS" "${TMP_DIR}/SHA256SUMS"; then
          verify_sha256 "$TMP_DIR" "$tarball" "${TMP_DIR}/SHA256SUMS"
          tar -xzf "${TMP_DIR}/${tarball}" -C "$TMP_DIR"
          install -m 0755 "${TMP_DIR}/kavach" "${dest}/kavach"
          install -m 0755 "${TMP_DIR}/kavach-recorder" "${dest}/kavach-recorder"
        else
          say "release asset download failed; falling back to go install"
          install_from_source "$dest"
        fi
      fi
    fi
  fi

  say "installed kavach and kavach-recorder to ${dest}"

  if [ "$SKIP_SKILLS" != "1" ]; then
    install_skills "$TMP_DIR"
  fi

  case ":${PATH}:" in
    *":${dest}:"*) ;;
    *)
      say "note: add ${dest} to your PATH:"
      say "  export PATH=\"${dest}:\$PATH\""
      ;;
  esac
}

main "$@"

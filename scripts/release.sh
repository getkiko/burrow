#!/usr/bin/env bash
#
# Build the Burrow release assets and publish them as a GitHub release.
#
# Usage:
#   TAG=v0.1.0 ./scripts/release.sh
#   ./scripts/release.sh v0.1.0
#
# Environment:
#   TAG        release tag (or pass it as the first argument)
#   DRY_RUN=1  only build dist/, do not publish
#
# Assets written to dist/:
#   burrow-signal-linux-amd64  burrow-signal-linux-arm64
#   burrow-linux-amd64         burrow-linux-arm64
#   burrow-darwin-amd64        burrow-darwin-arm64
#   SHA256SUMS
set -euo pipefail

# Build from this module alone, ignoring any go.work in a parent directory.
export GOWORK=off

REPO="getkiko/burrow"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${REPO_ROOT}/dist"

die() { echo "error: $*" >&2; exit 1; }

TAG="${1:-${TAG:-}}"
[[ -n "${TAG}" ]] || die "TAG is required, e.g. TAG=v0.1.0 $0"
[[ "${TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || \
  die "invalid tag ${TAG}; expected something like v1.2.3"

command -v go >/dev/null 2>&1 || die "go is required"

# target: "<binary> <package> <goos> <goarch>"
TARGETS=(
  "burrow-signal ./cmd/burrow-signal linux amd64"
  "burrow-signal ./cmd/burrow-signal linux arm64"
  "burrow ./cmd/burrow linux amd64"
  "burrow ./cmd/burrow linux arm64"
  "burrow ./cmd/burrow darwin amd64"
  "burrow ./cmd/burrow darwin arm64"
)

rm -rf "${DIST_DIR}"
mkdir -p "${DIST_DIR}"
cd "${REPO_ROOT}"

echo "==> building ${TAG}"
for target in "${TARGETS[@]}"; do
  read -r name pkg goos goarch <<< "${target}"
  out="${DIST_DIR}/${name}-${goos}-${goarch}"
  # CGO_ENABLED=0 gives static binaries; -trimpath and -s -w strip local
  # paths and debug information.
  GOOS="${goos}" GOARCH="${goarch}" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=${TAG}" -o "${out}" "${pkg}" \
    || die "build failed: ${name} ${goos}/${goarch}"
  echo "    $(basename "${out}")"
done

echo "==> writing SHA256SUMS"
(
  cd "${DIST_DIR}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum burrow-* > SHA256SUMS
  else
    shasum -a 256 burrow-* > SHA256SUMS
  fi
)
cat "${DIST_DIR}/SHA256SUMS"

if [[ "${DRY_RUN:-0}" == "1" ]]; then
  echo "==> DRY_RUN=1: assets are in ${DIST_DIR}, nothing published"
  exit 0
fi

command -v gh >/dev/null 2>&1 || die "the gh CLI is required to publish"
gh auth status >/dev/null 2>&1 || die "gh is not logged in: run gh auth login"
if gh release view "${TAG}" --repo "${REPO}" >/dev/null 2>&1; then
  die "release ${TAG} already exists; published releases must not be overwritten"
fi

echo "==> creating release ${TAG} on ${REPO}"
gh release create "${TAG}" "${DIST_DIR}"/* \
  --repo "${REPO}" \
  --title "${TAG}" \
  --generate-notes

echo "==> done: https://github.com/${REPO}/releases/tag/${TAG}"

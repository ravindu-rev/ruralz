#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Installs the pinned CI tools into bin/ (`make tools`), or only the tools
# named as arguments. Tools never enter
# go.mod: golangci-lint is a release binary checked against a pinned SHA-256,
# and govulncheck is built with `go install`, which the Go checksum database
# verifies (docs/engineering/02-repository-layout-and-conventions.md).
set -euo pipefail

GOLANGCI_LINT_VERSION=2.13.2
GOVULNCHECK_VERSION=v1.8.0

# SHA-256 of each golangci-lint release archive, from the release's checksums file.
golangci_lint_sha256() {
  case "$1" in
    linux-amd64) echo 2277d43b98ec0054280f2ac26b53268bae97682444678a59a657dd565da021d6 ;;
    linux-arm64) echo a2a4e0065aa41be71f7c5ac90f271b61751331e5d04314e62afe4027855f0893 ;;
    darwin-amd64) echo 8a13aaf9cbbb1dee52824e862cf0d0720e5bb97c1f4260d1e51623a09492b57b ;;
    darwin-arm64) echo f4bf83f0b64f055c42b28fc9a38861839f69c096e61c788e72dfaae412011789 ;;
    *) echo "install-tools: no pinned golangci-lint for $1" >&2; return 1 ;;
  esac
}

root=$(cd "$(dirname "$0")/.." && pwd)
bin="$root/bin"
mkdir -p "$bin"

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

install_golangci_lint() {
  if [[ -x "$bin/golangci-lint" ]] && "$bin/golangci-lint" version 2>/dev/null | grep -q "version $GOLANGCI_LINT_VERSION "; then
    return
  fi
  local platform archive want tmp
  platform="$(go env GOOS)-$(go env GOARCH)"
  want=$(golangci_lint_sha256 "$platform")
  archive="golangci-lint-$GOLANGCI_LINT_VERSION-$platform.tar.gz"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' RETURN
  curl -fsSL -o "$tmp/$archive" \
    "https://github.com/golangci/golangci-lint/releases/download/v$GOLANGCI_LINT_VERSION/$archive"
  if [[ "$(sha256 "$tmp/$archive")" != "$want" ]]; then
    echo "install-tools: checksum mismatch for $archive" >&2
    exit 1
  fi
  tar -xzf "$tmp/$archive" -C "$tmp"
  install -m 0755 "$tmp/golangci-lint-$GOLANGCI_LINT_VERSION-$platform/golangci-lint" "$bin/golangci-lint"
  echo "installed golangci-lint $GOLANGCI_LINT_VERSION"
}

# govulncheck type-checks the standard library, so it is built with the same
# Go release as the module (the go.mod toolchain line, or the local Go).
install_govulncheck() {
  local gover
  gover=$(cd "$root" && go env GOVERSION)
  if [[ -x "$bin/govulncheck" ]] && "$bin/govulncheck" -version 2>/dev/null | grep -q "govulncheck@$GOVULNCHECK_VERSION" &&
    [[ "$(go version "$bin/govulncheck" | awk '{print $2}')" == "$gover" ]]; then
    return
  fi
  (cd "$root" && GOBIN="$bin" GOTOOLCHAIN="$gover" go install "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION")
  echo "installed govulncheck $GOVULNCHECK_VERSION built with $gover"
}

# With no arguments install every tool; otherwise only the named ones.
tools=("$@")
(( ${#tools[@]} )) || tools=(golangci-lint govulncheck)
for tool in "${tools[@]}"; do
  case "$tool" in
    golangci-lint) install_golangci_lint ;;
    govulncheck) install_govulncheck ;;
    *) echo "install-tools: unknown tool $tool" >&2; exit 2 ;;
  esac
done

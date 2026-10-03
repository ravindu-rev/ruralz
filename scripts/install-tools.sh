#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Installs the pinned CI tools into bin/ (`make tools`), or only the tools
# named as arguments. Tools never enter go.mod: release binaries and
# archives are checked against a pinned SHA-256 before anything is
# installed, and govulncheck is built with `go install`, which the Go
# checksum database verifies (docs/engineering/02-repository-layout-and-conventions.md,
# docs/engineering/01-tech-stack-and-libraries.md "CI tooling").
#
#   golangci-lint  stage 2 lint and format
#   govulncheck    stage 6 supply chain
#   actionlint     stage 2 workflow lint
#   promtool       stage 8 alert rule checks (from the Prometheus release archive)
#   oha, vegeta    stage 11 Latency job load tools (OQ-performance-budgets-and-benchmarking-1)
#   cosign, syft   stage 12 keyless signing and CycloneDX SBOMs (OQ-release-versioning-and-compatibility-3)
#
# Pins cover linux and darwin on amd64 and arm64. The SHA-256 values come
# from each release's checksums file; oha publishes none, so its values were
# recorded from the release assets when the version was pinned. TOOLS_BIN
# overrides the install directory (default bin/).
set -euo pipefail

GOLANGCI_LINT_VERSION=2.13.2
GOVULNCHECK_VERSION=v1.8.0
ACTIONLINT_VERSION=1.7.12
PROMETHEUS_VERSION=3.15.0
OHA_VERSION=1.16.0
VEGETA_VERSION=12.13.0
COSIGN_VERSION=3.1.3
SYFT_VERSION=1.52.0

# pin <tool> <os>-<arch> prints the SHA-256 of that tool's release asset.
pin() {
  case "$1 $2" in
    "golangci-lint linux-amd64") echo 2277d43b98ec0054280f2ac26b53268bae97682444678a59a657dd565da021d6 ;;
    "golangci-lint linux-arm64") echo a2a4e0065aa41be71f7c5ac90f271b61751331e5d04314e62afe4027855f0893 ;;
    "golangci-lint darwin-amd64") echo 8a13aaf9cbbb1dee52824e862cf0d0720e5bb97c1f4260d1e51623a09492b57b ;;
    "golangci-lint darwin-arm64") echo f4bf83f0b64f055c42b28fc9a38861839f69c096e61c788e72dfaae412011789 ;;
    "actionlint linux-amd64") echo 8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8 ;;
    "actionlint linux-arm64") echo 325e971b6ba9bfa504672e29be93c24981eeb1c07576d730e9f7c8805afff0c6 ;;
    "actionlint darwin-amd64") echo 5b44c3bc2255115c9b69e30efc0fecdf498fdb63c5d58e17084fd5f16324c644 ;;
    "actionlint darwin-arm64") echo aba9ced2dee8d27fecca3dc7feb1a7f9a52caefa1eb46f3271ea66b6e0e6953f ;;
    "promtool linux-amd64") echo 2a542df32eac02ee17b9d844fb2aa1de00dafa5476579ba8a3ba862e9d572ea0 ;;
    "promtool linux-arm64") echo f1f90ec08e849d494ca66c611470afc50192f0355f1a61c33f2cbde02d067823 ;;
    "promtool darwin-amd64") echo 2d79e744c2d7e505db936fbc898e05abc74fcb6e437c25befd26e9c9f00aa58b ;;
    "promtool darwin-arm64") echo 920df4d17e78b3b0175af144eb318b0c74d1cf7b1d1251b326966f0e81977260 ;;
    "oha linux-amd64") echo 620bb9e16fb53eabc9a3fc45f88bdb41fefa3fee5c05e75892011ce320391716 ;;
    "oha linux-arm64") echo 99a790eb8c3e0feaca974bd6b32f0f8d4426a0c5b289f39e833e5b2c7529cd39 ;;
    "oha darwin-amd64") echo 5ecbfc5233e3f1d30384e142a9a8dd7ebf9d3298ab22476fabac44a8f2117e08 ;;
    "oha darwin-arm64") echo 7dea53ecb8342a7a067e1976fd0aef44ac33d9cc6b1e65c53637ffb932da63c4 ;;
    "vegeta linux-amd64") echo e8759ce45c14e18374bdccd3ba6068197bc3a9f9b7e484db3837f701b9d12e61 ;;
    "vegeta linux-arm64") echo 950381173a5575e25e8e086f36fc03bf65d61a2433329b48e41e1cb5e4133bba ;;
    "vegeta darwin-amd64") echo 4e912c83ce07db4e1e394e1cbb657f2396dff2f7ed90f03869a184cc17d0f994 ;;
    "vegeta darwin-arm64") echo fc408e242c4f4839e6fe536dbf1130bb02f430134827f6d831bf367a0929a799 ;;
    "cosign linux-amd64") echo 4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71 ;;
    "cosign linux-arm64") echo c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a ;;
    "cosign darwin-amd64") echo 2347488e5d5b25336644024dfeca5601b190e91197a71a917bda44744aff106c ;;
    "cosign darwin-arm64") echo 5cf948c2f4dfe59687bdd0b8523709067383e03982cc543475c8a7dc70e92a76 ;;
    "syft linux-amd64") echo caeedb81fb0491615f1ebd1761e4145d41ee86dd2cc7bf80669f9f5ad9d6133d ;;
    "syft linux-arm64") echo c46d5e4c28e12aa4c5becfaa343ef1c7f89045b6b895f2c21d471c62db09c706 ;;
    "syft darwin-amd64") echo 56975f5d7ffa9846a1eaf64330647841b878097bc7e3730cb9325f93add96917 ;;
    "syft darwin-arm64") echo 014d561b6d13059124155f74a6c5a9a99501f5e209313638dd884f39eb418ee6 ;;
    *) echo "install-tools: no pinned $1 for $2" >&2; return 1 ;;
  esac
}

root=$(cd "$(dirname "$0")/.." && pwd)
bin=${TOOLS_BIN:-$root/bin}
mkdir -p "$bin"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# The host platform as <os>-<arch>, without needing Go.
platform() {
  local os arch
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) echo "install-tools: unsupported OS $(uname -s)" >&2; return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) echo "install-tools: unsupported architecture $(uname -m)" >&2; return 1 ;;
  esac
  echo "$os-$arch"
}

# fetch <url> <sha256> <file> downloads url to file and checks its digest.
fetch() {
  curl -fsSL --retry 3 -o "$3" "$1"
  local got
  got=$(sha256 "$3")
  if [[ "$got" != "$2" ]]; then
    echo "install-tools: checksum mismatch for $1: got $got, want $2" >&2
    exit 1
  fi
}

# current <tool> <version> succeeds when bin/<tool> already reports version.
current() {
  [[ -x "$bin/$1" ]] || return 1
  case "$1" in
    golangci-lint) "$bin/$1" version 2>/dev/null | grep -q "version $2 " ;;
    actionlint | oha | syft) "$bin/$1" --version 2>&1 | grep -q "$2" ;;
    cosign) "$bin/$1" version 2>&1 | grep -q "GitVersion: *v$2\$" ;;
    vegeta) "$bin/$1" -version 2>&1 | grep -q "$2" ;;
    promtool) "$bin/$1" --version 2>&1 | grep -q "version $2 " ;;
    *) return 1 ;;
  esac
}

# install_archive <tool> <version> <url> <member> installs member of a
# .tar.gz release archive as bin/<tool>.
install_archive() {
  local tool=$1 version=$2 url=$3 member=$4 plat want tmp
  current "$tool" "$version" && return
  plat=$(platform)
  want=$(pin "$tool" "$plat")
  tmp="$work/$tool"
  mkdir -p "$tmp"
  fetch "$url" "$want" "$tmp/archive.tar.gz"
  tar -xzf "$tmp/archive.tar.gz" -C "$tmp" "$member"
  install -m 0755 "$tmp/$member" "$bin/$tool"
  echo "installed $tool $version"
}

# install_binary <tool> <version> <url> installs a bare release binary.
install_binary() {
  local tool=$1 version=$2 url=$3 plat want tmp
  current "$tool" "$version" && return
  plat=$(platform)
  want=$(pin "$tool" "$plat")
  tmp="$work/$tool"
  mkdir -p "$tmp"
  fetch "$url" "$want" "$tmp/$tool"
  install -m 0755 "$tmp/$tool" "$bin/$tool"
  echo "installed $tool $version"
}

install_golangci_lint() {
  local plat
  plat=$(platform)
  install_archive golangci-lint "$GOLANGCI_LINT_VERSION" \
    "https://github.com/golangci/golangci-lint/releases/download/v$GOLANGCI_LINT_VERSION/golangci-lint-$GOLANGCI_LINT_VERSION-$plat.tar.gz" \
    "golangci-lint-$GOLANGCI_LINT_VERSION-$plat/golangci-lint"
}

install_actionlint() {
  local plat
  plat=$(platform)
  install_archive actionlint "$ACTIONLINT_VERSION" \
    "https://github.com/rhysd/actionlint/releases/download/v$ACTIONLINT_VERSION/actionlint_${ACTIONLINT_VERSION}_${plat/-/_}.tar.gz" \
    actionlint
}

install_promtool() {
  local plat
  plat=$(platform)
  install_archive promtool "$PROMETHEUS_VERSION" \
    "https://github.com/prometheus/prometheus/releases/download/v$PROMETHEUS_VERSION/prometheus-$PROMETHEUS_VERSION.${plat}.tar.gz" \
    "prometheus-$PROMETHEUS_VERSION.${plat}/promtool"
}

install_oha() {
  local plat
  plat=$(platform)
  # oha names its macOS assets "macos".
  install_binary oha "$OHA_VERSION" \
    "https://github.com/hatoo/oha/releases/download/v$OHA_VERSION/oha-${plat/darwin/macos}"
}

install_vegeta() {
  local plat
  plat=$(platform)
  install_archive vegeta "$VEGETA_VERSION" \
    "https://github.com/tsenart/vegeta/releases/download/v$VEGETA_VERSION/vegeta_${VEGETA_VERSION}_${plat/-/_}.tar.gz" \
    vegeta
}

install_cosign() {
  local plat
  plat=$(platform)
  install_binary cosign "$COSIGN_VERSION" \
    "https://github.com/sigstore/cosign/releases/download/v$COSIGN_VERSION/cosign-$plat"
}

install_syft() {
  local plat
  plat=$(platform)
  install_archive syft "$SYFT_VERSION" \
    "https://github.com/anchore/syft/releases/download/v$SYFT_VERSION/syft_${SYFT_VERSION}_${plat/-/_}.tar.gz" \
    syft
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

# With no arguments install the tools of `make tools` (stages 2, 6 and 8);
# otherwise only the named ones.
tools=("$@")
(( ${#tools[@]} )) || tools=(golangci-lint govulncheck actionlint promtool)
for tool in "${tools[@]}"; do
  case "$tool" in
    golangci-lint) install_golangci_lint ;;
    govulncheck) install_govulncheck ;;
    actionlint) install_actionlint ;;
    promtool) install_promtool ;;
    oha) install_oha ;;
    vegeta) install_vegeta ;;
    cosign) install_cosign ;;
    syft) install_syft ;;
    *) echo "install-tools: unknown tool $tool" >&2; exit 2 ;;
  esac
done

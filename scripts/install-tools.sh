#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Installs the pinned CI tools into bin/ (`make tools`), or only the tools
# named as arguments. Tools never enter go.mod: release binaries and
# archives are checked against a pinned SHA-256 before anything is
# installed, and govulncheck and the protoc plugins are built with
# `go install`, which the Go checksum database verifies
# (docs/engineering/02-repository-layout-and-conventions.md,
# docs/engineering/01-tech-stack-and-libraries.md "CI tooling").
#
#   golangci-lint  make lint: lint and format
#   govulncheck    make supply-chain
#   actionlint     make lint: workflow lint, running the pinned shellcheck
#                  on the workflows' run steps, the same version on every
#                  host
#   buf            make lint and make generate: the buf rules checks and
#                  the protobuf code generation
#   protoc-gen-go, protoc-gen-connect-go
#                  buf's local plugins, built from the module at the go.mod
#                  version of google.golang.org/protobuf and of
#                  connectrpc.com/connect, so generated code and runtime
#                  move together
#   promtool       make integration: the alert rule tests (from the
#                  Prometheus release archive)
#   cosign, syft   the release: keyless signing and CycloneDX SBOMs
#
# Pins cover linux and darwin on amd64 and arm64. The SHA-256 values come
# from each release's checksums file; shellcheck publishes none, so its
# values were recorded from the release assets when the version was
# pinned. TOOLS_BIN overrides the install directory (default bin/).
set -euo pipefail

GOLANGCI_LINT_VERSION=2.15.0
GOVULNCHECK_VERSION=v1.8.0
ACTIONLINT_VERSION=1.7.12
SHELLCHECK_VERSION=0.11.0
BUF_VERSION=1.73.0
PROMETHEUS_VERSION=3.15.0
COSIGN_VERSION=3.1.3
SYFT_VERSION=1.52.0

# pin <tool> <os>-<arch> prints the SHA-256 of that tool's release asset.
pin() {
  case "$1 $2" in
    "golangci-lint linux-amd64") echo 9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea ;;
    "golangci-lint linux-arm64") echo b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998 ;;
    "golangci-lint darwin-amd64") echo 767b899a28285e88ded2823d5dcc4bc5ef681173eb2d80f029a7b17ee4ece3b8 ;;
    "golangci-lint darwin-arm64") echo d1e409481a8ca846c4bc97e781eb5cb1bd5c23353a0b3977a1cbda1b1b6447d5 ;;
    "actionlint linux-amd64") echo 8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8 ;;
    "actionlint linux-arm64") echo 325e971b6ba9bfa504672e29be93c24981eeb1c07576d730e9f7c8805afff0c6 ;;
    "actionlint darwin-amd64") echo 5b44c3bc2255115c9b69e30efc0fecdf498fdb63c5d58e17084fd5f16324c644 ;;
    "actionlint darwin-arm64") echo aba9ced2dee8d27fecca3dc7feb1a7f9a52caefa1eb46f3271ea66b6e0e6953f ;;
    "shellcheck linux-amd64") echo b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6 ;;
    "shellcheck linux-arm64") echo 68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc ;;
    "shellcheck darwin-amd64") echo c2c15e08df0e8fbc374c335b230a7ee958c313fa5714817a59aa59f1aa594f51 ;;
    "shellcheck darwin-arm64") echo 339b930feb1ea764467013cc1f72d09cd6b869ebf1013296ba9055ab2ffbd26f ;;
    "buf linux-amd64") echo 8f2986298ad08f0cc1bf999b9797b7c383adf32d7edf0f73d6f1e1a701baeac1 ;;
    "buf linux-arm64") echo 902b75267db7f4391e99b7fa0756050e5354234cc0437ef50eee9c788950c7a3 ;;
    "buf darwin-amd64") echo ff78d0ebf34180ebfa81d370275851ec630fcb088bf33e213fd723d0fd7444a6 ;;
    "buf darwin-arm64") echo 6e6df0fef4522e4e43dfe7c341873c3f2c29ceb45a9dfa5e0bad5580b8b2022f ;;
    "promtool linux-amd64") echo 2a542df32eac02ee17b9d844fb2aa1de00dafa5476579ba8a3ba862e9d572ea0 ;;
    "promtool linux-arm64") echo f1f90ec08e849d494ca66c611470afc50192f0355f1a61c33f2cbde02d067823 ;;
    "promtool darwin-amd64") echo 2d79e744c2d7e505db936fbc898e05abc74fcb6e437c25befd26e9c9f00aa58b ;;
    "promtool darwin-arm64") echo 920df4d17e78b3b0175af144eb318b0c74d1cf7b1d1251b326966f0e81977260 ;;
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
    actionlint | syft) "$bin/$1" --version 2>&1 | grep -q "$2" ;;
    shellcheck) "$bin/$1" --version 2>&1 | grep -qx "version: $2" ;;
    buf) "$bin/$1" --version 2>&1 | grep -qx "$2" ;;
    cosign) "$bin/$1" version 2>&1 | grep -q "GitVersion: *v$2\$" ;;
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

install_shellcheck() {
  local plat arch
  plat=$(platform)
  # The shellcheck release assets name the architectures x86_64 and aarch64.
  arch=${plat#*-}
  case "$arch" in
    amd64) arch=x86_64 ;;
    arm64) arch=aarch64 ;;
  esac
  install_archive shellcheck "$SHELLCHECK_VERSION" \
    "https://github.com/koalaman/shellcheck/releases/download/v$SHELLCHECK_VERSION/shellcheck-v$SHELLCHECK_VERSION.${plat%-*}.$arch.tar.gz" \
    "shellcheck-v$SHELLCHECK_VERSION/shellcheck"
}

install_promtool() {
  local plat
  plat=$(platform)
  install_archive promtool "$PROMETHEUS_VERSION" \
    "https://github.com/prometheus/prometheus/releases/download/v$PROMETHEUS_VERSION/prometheus-$PROMETHEUS_VERSION.${plat}.tar.gz" \
    "prometheus-$PROMETHEUS_VERSION.${plat}/promtool"
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

install_buf() {
  local plat os arch
  plat=$(platform)
  # The buf release assets name the systems Linux and Darwin and the
  # architectures x86_64, and aarch64 on Linux but arm64 on Darwin.
  case "${plat%-*}" in
    linux) os=Linux ;;
    darwin) os=Darwin ;;
  esac
  case "$plat" in
    *-amd64) arch=x86_64 ;;
    linux-arm64) arch=aarch64 ;;
    darwin-arm64) arch=arm64 ;;
  esac
  install_binary buf "$BUF_VERSION" \
    "https://github.com/bufbuild/buf/releases/download/v$BUF_VERSION/buf-$os-$arch"
}

# install_protoc_plugin <tool> <module> <package> builds a protoc plugin from
# the module at its go.mod version, which a dependency update moves, so the
# check compares the installed binary's version with go.mod each time.
# protoc-gen-go prints "protoc-gen-go v1.36.12", protoc-gen-connect-go
# "1.21.1".
install_protoc_plugin() {
  local tool=$1 module=$2 pkg=$3 want got
  want=$(cd "$root" && go list -m -f '{{.Version}}' "$module")
  if [[ -x "$bin/$tool" ]]; then
    got=$("$bin/$tool" --version 2>/dev/null || true)
    if [[ "$got" == "$tool $want" || "$got" == "${want#v}" ]]; then
      return
    fi
  fi
  (cd "$root" && GOBIN="$bin" go install "$pkg")
  echo "installed $tool $want"
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

# With no arguments install the tools of `make tools` (make lint, generate,
# supply-chain and integration); otherwise only the named ones.
tools=("$@")
(( ${#tools[@]} )) || tools=(golangci-lint govulncheck actionlint shellcheck buf protoc-gen-go protoc-gen-connect-go promtool)
for tool in "${tools[@]}"; do
  case "$tool" in
    golangci-lint) install_golangci_lint ;;
    govulncheck) install_govulncheck ;;
    actionlint) install_actionlint ;;
    shellcheck) install_shellcheck ;;
    buf) install_buf ;;
    protoc-gen-go) install_protoc_plugin protoc-gen-go google.golang.org/protobuf google.golang.org/protobuf/cmd/protoc-gen-go ;;
    protoc-gen-connect-go) install_protoc_plugin protoc-gen-connect-go connectrpc.com/connect connectrpc.com/connect/cmd/protoc-gen-connect-go ;;
    promtool) install_promtool ;;
    cosign) install_cosign ;;
    syft) install_syft ;;
    *) echo "install-tools: unknown tool $tool" >&2; exit 2 ;;
  esac
done

#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Refreshes the vendored upstream conformance suites of CI stage 8 at their
# pinned versions (ADR-0003 "Confirmation" upstream suites; 11 req 18):
#
#   scripts/update-test-suites.sh [yaml] [json-schema]   (default: both)
#   scripts/update-test-suites.sh --digest URL COMMIT    (prints a new pin)
#
# Layout (FIXTURES_ROOT overrides test/fixtures):
#
#   yaml-test-suite/           YAML Test Suite data release, run against the
#     data/<ID>/...            restricted profile (internal/config/profile,
#     LICENSE                  integration tag) with its expected-failures.txt
#     SOURCE                   ratchet; the release's name/ and tags/ symlink
#                              indexes are left out
#   json-schema-test-suite/    JSON-Schema-Test-Suite, run against
#     data/tests/draft2020-12/ jsonschema/v6 (internal/config/schemaview);
#     data/remotes/            the runner skips optional/ except the formats
#     LICENSE                  Ruralz uses; remotes/ serves the
#     SOURCE                   http://localhost:1234/ references
#
# Only data/, LICENSE and SOURCE are replaced; other files in a suite
# directory (expected-failures.txt) are kept. Both suites are MIT licensed.
#
# Integrity: each suite is pinned by commit and by the SHA-256 of
# `git archive --format=tar <commit>`, which is deterministic for a commit
# (GitHub's compressed archive downloads are not guaranteed byte-stable).
# The script fetches the pinned commit with git, rebuilds that tarball,
# checks its digest, then extracts it. To move a pin, print the new digest
# with --digest, then update the *_COMMIT and *_SHA256 lines below and
# rerun the script.
set -euo pipefail

YAML_NAME="YAML Test Suite"
YAML_URL=https://github.com/yaml/yaml-test-suite
YAML_REF=data-2022-01-17
YAML_COMMIT=6e6c296ae9c9d2d5c4134b4b64d01b29ac19ff6f
YAML_SHA256=a3c650ace58892c224040216860ada4fa76596989f3c61c6f972d93a188144ca
# The data release carries no license file; it is the one of the source
# release it was generated from (tag v2022-01-17).
YAML_LICENSE_REF=v2022-01-17
YAML_LICENSE_COMMIT=45db50aecf9b1520f8258938c88f396e96f30831
YAML_LICENSE_PATH=License
YAML_LICENSE_SHA256=c9562189164244554a69ab3f29d2d93ed9492c165723aaaa5fffc932cdbbfc85

JSON_NAME="JSON-Schema-Test-Suite"
JSON_URL=https://github.com/json-schema-org/JSON-Schema-Test-Suite
JSON_REF=main
JSON_COMMIT=5b0ee1613e45fcc2bddac00e07c19cd49b00d8a8
JSON_SHA256=5477d894cb15fc844862d0baf5d0dfbd26b5a5f112087350a6802289b089c3b1
JSON_LICENSE_PATH=LICENSE

root=$(cd "$(dirname "$0")/.." && pwd)
fixtures=${FIXTURES_ROOT:-$root/test/fixtures}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

check() {
  local got
  got=$(sha256 "$1")
  if [[ "$got" != "$2" ]]; then
    echo "update-test-suites: SHA-256 mismatch for $3: got $got, want $2" >&2
    exit 1
  fi
}

# fetch <url> <dir> <commit>... fetches the commits into a new repository.
fetch() {
  local url=$1 dir=$2
  shift 2
  git init --quiet "$dir"
  git -C "$dir" fetch --quiet --depth 1 "$url.git" "$@"
}

# archive <dir> <commit> <out> writes the commit's tarball.
archive() {
  git -C "$1" archive --format=tar -o "$3" "$2"
}

# write_source <dest> <name> <url> <ref> <commit> <sha256> <license note>
write_source() {
  cat >"$1/SOURCE" <<EOF
name: $2
url: $3
ref: $4
commit: $5
sha256: $6
sha256-of: git archive --format=tar $5
license: MIT ($7)
updated-by: scripts/update-test-suites.sh
EOF
}

# replace <dest> <staging>: swap in the staged data/ directory.
replace() {
  mkdir -p "$1"
  rm -rf "$1/data"
  mv "$2" "$1/data"
}

vendor_yaml() {
  local dest="$fixtures/yaml-test-suite" repo="$work/yaml" tar="$work/yaml.tar" stage="$work/yaml-data"
  fetch "$YAML_URL" "$repo" "$YAML_COMMIT" "$YAML_LICENSE_COMMIT"
  archive "$repo" "$YAML_COMMIT" "$tar"
  check "$tar" "$YAML_SHA256" "$YAML_NAME $YAML_REF"
  git -C "$repo" show "$YAML_LICENSE_COMMIT:$YAML_LICENSE_PATH" >"$work/yaml.license"
  check "$work/yaml.license" "$YAML_LICENSE_SHA256" "$YAML_NAME $YAML_LICENSE_REF:$YAML_LICENSE_PATH"
  mkdir -p "$stage"
  tar -xf "$tar" -C "$stage"
  rm -rf "$stage/name" "$stage/tags"
  replace "$dest" "$stage"
  cp "$work/yaml.license" "$dest/LICENSE"
  write_source "$dest" "$YAML_NAME" "$YAML_URL" "$YAML_REF" "$YAML_COMMIT" "$YAML_SHA256" \
    "$YAML_LICENSE_PATH of $YAML_LICENSE_REF, commit $YAML_LICENSE_COMMIT, sha256 $YAML_LICENSE_SHA256"
  echo "vendored $YAML_NAME $YAML_REF into ${dest#"$root"/}"
}

vendor_json_schema() {
  local dest="$fixtures/json-schema-test-suite" repo="$work/json" tar="$work/json.tar" stage="$work/json-data"
  fetch "$JSON_URL" "$repo" "$JSON_COMMIT"
  archive "$repo" "$JSON_COMMIT" "$tar"
  check "$tar" "$JSON_SHA256" "$JSON_NAME $JSON_COMMIT"
  mkdir -p "$stage"
  tar -xf "$tar" -C "$stage" tests/draft2020-12 remotes
  tar -xf "$tar" -C "$work" "$JSON_LICENSE_PATH"
  replace "$dest" "$stage"
  cp "$work/$JSON_LICENSE_PATH" "$dest/LICENSE"
  write_source "$dest" "$JSON_NAME" "$JSON_URL" "$JSON_REF" "$JSON_COMMIT" "$JSON_SHA256" \
    "$JSON_LICENSE_PATH of the same commit"
  echo "vendored $JSON_NAME $JSON_COMMIT into ${dest#"$root"/}"
}

if [[ "${1:-}" == --digest ]]; then
  if (($# != 3)); then
    echo "usage: update-test-suites.sh --digest URL COMMIT" >&2
    exit 2
  fi
  fetch "${2%.git}" "$work/digest" "$3"
  archive "$work/digest" "$3" "$work/digest.tar"
  echo "$(sha256 "$work/digest.tar")  git archive --format=tar $3"
  exit 0
fi

suites=("$@")
((${#suites[@]})) || suites=(yaml json-schema)
for suite in "${suites[@]}"; do
  case "$suite" in
    yaml) vendor_yaml ;;
    json-schema) vendor_json_schema ;;
    *)
      echo "update-test-suites: unknown suite $suite (want yaml or json-schema)" >&2
      exit 2
      ;;
  esac
done

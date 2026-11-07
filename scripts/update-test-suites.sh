#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Refreshes the vendored upstream conformance suites that the tests run at
# their pinned versions: the integration tests' YAML Test Suite and
# JSON-Schema-Test-Suite (ADR-0003 "Confirmation" upstream suites) and the
# AWS signing test suite that the auth.upstream-sigv4 unit tests sign
# (docs/engineering/03-testing-and-quality-strategy.md "Conformance
# suites"):
#
#   scripts/update-test-suites.sh [yaml] [json-schema] [aws-signing]
#                                                      (default: all three)
#   scripts/update-test-suites.sh --digest URL COMMIT  (prints a new pin)
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
# The AWS signing test suite (aws-c-auth, Apache-2.0) lands beside its
# tests, in internal/filter/auth/upstreamsigv4/testdata/aws-signing-test-suite:
#
#   v4/<case>/                 context.json, request.txt,
#                              header-canonical-request.txt and
#                              header-signed-request.txt of every
#                              Signature Version 4 case but the
#                              -unnormalized ones and post-sts-header-after,
#                              which sign in modes no Ruralz service uses
#                              (vectors_test.go says why); the query-signing
#                              files are left out
#   LICENSE, NOTICE, SOURCE    the license and notice of the same commit
#
# v4/, LICENSE, NOTICE and SOURCE are replaced.
#
# Integrity: each suite is pinned by commit and by the SHA-256 of
# `git archive --format=tar <commit>` with no line-ending conversion
# (core.autocrlf=false, core.eol=lf), which is deterministic for a commit
# on every OS (GitHub's compressed archive downloads are not guaranteed
# byte-stable, and a checkout's core.autocrlf would rewrite the text files
# of the tarball).
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

AWS_NAME="AWS signing test suite, Signature Version 4 cases"
AWS_URL=https://github.com/awslabs/aws-c-auth
AWS_REF=v1.0.3
AWS_COMMIT=6f9496f2f355bf7f53083ea759ae17d5fdd366b2
AWS_SHA256=ee570550905ec3cce5dcd20d5eff333285f2c199fff7373f466d3db79af896e8
AWS_PATH=tests/aws-signing-test-suite/v4
# The files of each kept case, and the cases left out.
AWS_FILES=(context.json request.txt header-canonical-request.txt header-signed-request.txt)
AWS_SKIP_SUFFIX=-unnormalized
AWS_SKIP_CASES=(post-sts-header-after)

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

# archive <dir> <commit> <out> writes the commit's tarball, its text files
# exactly as committed whatever the OS and the user's git configuration.
archive() {
  git -C "$1" -c core.autocrlf=false -c core.eol=lf archive --format=tar -o "$3" "$2"
}

# write_source <dest> <name> <url> <ref> <commit> <sha256> <license note>
write_source() {
  cat >"$1/SOURCE" <<EOF
name: $2
url: $3
ref: $4
commit: $5
sha256: $6
sha256-of: git -c core.autocrlf=false -c core.eol=lf archive --format=tar $5
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

vendor_aws_signing() {
  local dest="$root/internal/filter/auth/upstreamsigv4/testdata/aws-signing-test-suite"
  local repo="$work/aws" tar="$work/aws.tar" stage="$work/aws-data" cases="$work/aws-v4"
  fetch "$AWS_URL" "$repo" "$AWS_COMMIT"
  archive "$repo" "$AWS_COMMIT" "$tar"
  check "$tar" "$AWS_SHA256" "$AWS_NAME $AWS_REF"
  mkdir -p "$stage" "$cases"
  tar -xf "$tar" -C "$stage" "$AWS_PATH" LICENSE NOTICE
  local dir name skip file
  for dir in "$stage/$AWS_PATH"/*/; do
    name=$(basename "$dir")
    [[ "$name" == *"$AWS_SKIP_SUFFIX" ]] && continue
    skip=false
    for file in "${AWS_SKIP_CASES[@]}"; do
      [[ "$name" == "$file" ]] && skip=true
    done
    $skip && continue
    mkdir "$cases/$name"
    for file in "${AWS_FILES[@]}"; do
      cp "$dir/$file" "$cases/$name/$file"
    done
  done
  mkdir -p "$dest"
  rm -rf "$dest/v4"
  mv "$cases" "$dest/v4"
  cp "$stage/LICENSE" "$dest/LICENSE"
  cp "$stage/NOTICE" "$dest/NOTICE"
  cat >"$dest/SOURCE" <<EOF
name: $AWS_NAME
url: $AWS_URL
ref: $AWS_REF
commit: $AWS_COMMIT
path: $AWS_PATH
sha256: $AWS_SHA256
sha256-of: git -c core.autocrlf=false -c core.eol=lf archive --format=tar $AWS_COMMIT
license: Apache-2.0 (LICENSE and NOTICE of the same commit)
kept: ${AWS_FILES[*]} of every case but the query-signing files, the unnormalized cases and ${AWS_SKIP_CASES[*]} (vectors_test.go says why)
updated-by: scripts/update-test-suites.sh
EOF
  echo "vendored $AWS_NAME $AWS_REF into ${dest#"$root"/}"
}

if [[ "${1:-}" == --digest ]]; then
  if (($# != 3)); then
    echo "usage: update-test-suites.sh --digest URL COMMIT" >&2
    exit 2
  fi
  fetch "${2%.git}" "$work/digest" "$3"
  archive "$work/digest" "$3" "$work/digest.tar"
  echo "$(sha256 "$work/digest.tar")  git -c core.autocrlf=false -c core.eol=lf archive --format=tar $3"
  exit 0
fi

suites=("$@")
((${#suites[@]})) || suites=(yaml json-schema aws-signing)
for suite in "${suites[@]}"; do
  case "$suite" in
    yaml) vendor_yaml ;;
    json-schema) vendor_json_schema ;;
    aws-signing) vendor_aws_signing ;;
    *)
      echo "update-test-suites: unknown suite $suite (want yaml, json-schema or aws-signing)" >&2
      exit 2
      ;;
  esac
done

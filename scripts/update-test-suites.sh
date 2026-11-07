#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Refreshes the vendored upstream conformance suites that the integration
# tests run at their pinned versions (ADR-0003 "Confirmation" upstream
# suites), and the SM-8 OpenAPI corpus (docs/vision/01-vision-and-
# positioning.md "Success metrics") that ruralz bundle import openapi runs:
#
#   scripts/update-test-suites.sh [yaml] [json-schema] [openapi]   (default: all)
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
#   openapi-corpus/<entry>/    one public OpenAPI 3.0 or 3.1 document per
#     data/...                 entry, with the files its relative $refs name
#     LICENSE                  at their upstream relative paths, the
#     NOTICE                   upstream license text, and the upstream
#     SOURCE                   NOTICE when the work has one
#
# Only data/, LICENSE and SOURCE are replaced, and of an OpenAPI corpus
# entry only data/, LICENSE and NOTICE; other files in a suite directory
# (expected-failures.txt) are kept. The YAML and JSON Schema suites are MIT
# licensed. An OpenAPI corpus entry's SOURCE is its pin, read and kept: it
# names the entry's repository, commit, the upstream directory its data/
# mirrors (base), its root document, every file with its SHA-256, its
# license (an SPDX identifier and the license file's path at the commit)
# with that file's SHA-256, and, for a work with a NOTICE, the notice's
# path and SHA-256, so adding an entry is adding its SOURCE.
#
# Integrity: the YAML and JSON Schema suites are pinned by commit and by the
# SHA-256 of `git archive --format=tar <commit>`, which is deterministic for
# a commit (GitHub's compressed archive downloads are not guaranteed
# byte-stable). The script fetches the pinned commit with git, rebuilds that
# tarball, checks its digest, then extracts it. To move a pin, print the new
# digest with --digest, then update the *_COMMIT and *_SHA256 lines below and
# rerun the script. The OpenAPI corpus pins each file instead, since its
# entries come from many repositories, some of them large: each file and
# license is downloaded from raw.githubusercontent.com at the pinned commit,
# which serves a commit's blobs byte for byte, and checked against its
# SHA-256 before anything is replaced.
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

OPENAPI_RAW=https://raw.githubusercontent.com

# source_field <SOURCE> <key> prints the value of the SOURCE file's first
# "<key>: " line, and fails when it has none.
source_field() {
  local line
  while IFS= read -r line; do
    if [[ "$line" == "$2: "* ]]; then
      printf '%s\n' "${line#"$2: "}"
      return 0
    fi
  done <"$1"
  echo "update-test-suites: ${1#"$root"/} has no $2 line" >&2
  exit 1
}

# safe_path <path> <what> fails unless path is relative and stays below the
# directory it is joined to: no leading /, no backslash and no . or ..
# segment.
safe_path() {
  local seg
  local -a segs
  if [[ -z "$1" || "$1" == /* || "$1" == *\\* ]]; then
    echo "update-test-suites: $2: path $1 is not a relative slash path" >&2
    exit 1
  fi
  IFS=/ read -r -a segs <<<"$1"
  for seg in "${segs[@]}"; do
    if [[ -z "$seg" || "$seg" == . || "$seg" == .. ]]; then
      echo "update-test-suites: $2: path $1 has an empty, . or .. segment" >&2
      exit 1
    fi
  done
}

# download <url> <out> downloads one file, creating out's directory.
download() {
  mkdir -p "$(dirname "$2")"
  curl -fsSL --retry 3 -o "$2" "$1"
}

vendor_openapi() {
  local corpus="$fixtures/openapi-corpus" src entry url repo commit base prefix lic lic_path lic_sum notice stage line rel sum n=0
  for src in "$corpus"/*/SOURCE; do
    [[ -f "$src" ]] || continue
    entry=$(basename "$(dirname "$src")")
    url=$(source_field "$src" url)
    repo=${url#https://github.com/}
    if [[ "$repo" == "$url" || "$repo" != */* ]]; then
      echo "update-test-suites: $entry: url $url is not a GitHub repository" >&2
      exit 1
    fi
    commit=$(source_field "$src" commit)
    if [[ ! "$commit" =~ ^[0-9a-f]{40}$ ]]; then
      echo "update-test-suites: $entry: commit $commit is not a full commit SHA" >&2
      exit 1
    fi
    base=$(source_field "$src" base)
    prefix=""
    if [[ "$base" != . ]]; then
      safe_path "$base" "$entry base"
      prefix="$base/"
    fi
    lic=$(source_field "$src" license)
    lic_path=${lic##*\(}
    lic_path=${lic_path%\)}
    safe_path "$lic_path" "$entry license"
    lic_sum=$(source_field "$src" license-sha256)
    stage="$work/openapi/$entry"
    mkdir -p "$stage/data"
    while IFS= read -r line; do
      [[ "$line" == "file: "* ]] || continue
      line=${line#file: }
      rel=${line%% sha256 *}
      sum=${line##* sha256 }
      safe_path "$rel" "$entry file"
      download "$OPENAPI_RAW/$repo/$commit/$prefix$rel" "$stage/data/$rel"
      check "$stage/data/$rel" "$sum" "$entry $rel"
    done <"$src"
    download "$OPENAPI_RAW/$repo/$commit/$lic_path" "$stage/LICENSE"
    check "$stage/LICENSE" "$lic_sum" "$entry $lic_path"
    # An Apache-2.0 work's NOTICE travels with every copy (section 4(d)).
    notice=""
    if grep -q '^notice: ' "$src"; then
      notice=$(source_field "$src" notice)
      safe_path "$notice" "$entry notice"
      download "$OPENAPI_RAW/$repo/$commit/$notice" "$stage/NOTICE"
      check "$stage/NOTICE" "$(source_field "$src" notice-sha256)" "$entry $notice"
    fi
    replace "$corpus/$entry" "$stage/data"
    cp "$stage/LICENSE" "$corpus/$entry/LICENSE"
    if [[ -n "$notice" ]]; then
      cp "$stage/NOTICE" "$corpus/$entry/NOTICE"
    fi
    n=$((n + 1))
  done
  if ((n == 0)); then
    echo "update-test-suites: no OpenAPI corpus entry under ${corpus#"$root"/}" >&2
    exit 1
  fi
  echo "vendored $n OpenAPI corpus entries into ${corpus#"$root"/}"
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
((${#suites[@]})) || suites=(yaml json-schema openapi)
for suite in "${suites[@]}"; do
  case "$suite" in
    yaml) vendor_yaml ;;
    json-schema) vendor_json_schema ;;
    openapi) vendor_openapi ;;
    *)
      echo "update-test-suites: unknown suite $suite (want yaml, json-schema or openapi)" >&2
      exit 2
      ;;
  esac
done

#!/usr/bin/env bash
# sync-versions.sh — refresh the vendored snapshot of meta-harness's pin file.
#
# pkg/versions/versions.json and meta-harness's src/versions/versions.json must
# stay in cross-repo parity. This repo vendors a snapshot of meta-harness's file
# at pkg/versions/testdata/meta-harness-versions.json; the hermetic parity test
# (pkg/versions/parity_test.go) asserts the embedded pins semantically match it.
# The two repos format the file differently (compact vs pretty-printed), so all
# comparisons here are format-insensitive (jq -S normalized), never byte-level.
#
# This is a dev-machine tool — it reads a sibling meta-harness checkout and is
# never invoked by `make test`. The outward push (meta-harness pulling THIS
# repo's file) is META-HARNESS-91's symmetric script, not a mode here.
#
# Usage:
#   scripts/sync-versions.sh            copy the sibling's versions.json over the vendored snapshot
#   scripts/sync-versions.sh --check    exit 1 if the vendored snapshot differs semantically from the sibling's file
#
# The sibling checkout defaults to ~/Work/aether/meta-harness; override with
# META_HARNESS_DIR.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendored="$here/pkg/versions/testdata/meta-harness-versions.json"
sibling="${META_HARNESS_DIR:-$HOME/Work/aether/meta-harness}/src/versions/versions.json"

mode="sync"
while [ $# -gt 0 ]; do
  case "$1" in
    --check) mode="check" ;;
    *) echo "unknown arg: $1 (usage: sync-versions.sh [--check])" >&2; exit 2 ;;
  esac; shift
done

if [ ! -f "$sibling" ]; then
  echo "sibling checkout not found: $sibling" >&2
  echo "clone meta-harness there or set META_HARNESS_DIR to its checkout" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required for format-insensitive comparison (brew install jq)" >&2
  exit 1
fi

if [ "$mode" = check ]; then
  if [ ! -f "$vendored" ] || ! diff -u <(jq -S . "$vendored") <(jq -S . "$sibling"); then
    echo "versions DRIFT: $vendored differs semantically from $sibling" >&2
    echo "Run scripts/sync-versions.sh, re-align pkg/versions/versions.json, and commit." >&2
    exit 1
  fi
  echo "versions parity OK"
  exit 0
fi

cp "$sibling" "$vendored"
echo "wrote $vendored (from $sibling)"
if ! diff -q <(jq -S . "$vendored") <(jq -S . "$here/pkg/versions/versions.json") >/dev/null; then
  echo "note: pkg/versions/versions.json now differs from the refreshed snapshot —" >&2
  echo "      update the pins there too or the parity test will fail." >&2
fi

#!/usr/bin/env bash
# sync-models-corpus.sh — regenerate the models-corpus manifest and verify it
# against the canonical harness-wrapper copy.
#
# The model-picker conformance corpus (test/corpus/models) is vendored
# BYTE-IDENTICALLY into harness-wrapper (CANONICAL) and meta-harness (this repo).
# The two repos are "in sync" iff their committed test/corpus/models/
# MANIFEST.sha256 are byte-equal. harness-wrapper owns the fixtures and mirrors
# them outward with its own scripts/sync-models-corpus.sh --to <this-repo>.
#
# This is the SYMMETRIC guard (the META-HARNESS-91 analogue): harness-wrapper's
# parity/corpus test only catches harness-wrapper drifting from the vendored
# snapshot. The meta-harness-drifts-first direction must be enforced HERE, so
# --check recomputes this repo's manifest (catches a local fixture edit that
# forgot to regenerate), and --against <harness-wrapper> diffs the two committed
# manifests (catches this repo drifting from the canonical copy).
#
# Usage:
#   scripts/sync-models-corpus.sh                  regenerate MANIFEST.sha256 in place
#   scripts/sync-models-corpus.sh --check          verify MANIFEST.sha256 is current (CI); exit 1 on drift
#   scripts/sync-models-corpus.sh --against DIR     also assert this manifest == DIR's committed manifest
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
corpus="$here/test/corpus/models"
manifest="$corpus/MANIFEST.sha256"

sha256() { # file bytes on stdin -> lowercase hex digest
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | awk '{print $1}'
  else shasum -a 256 | awk '{print $1}'; fi
}

gen_manifest() { # -> "<hex>  <relpath>" per file, sorted, excluding the manifest
  ( cd "$corpus"
    find . -type f ! -name MANIFEST.sha256 | LC_ALL=C sort | while read -r f; do
      printf '%s  %s\n' "$(sha256 < "$f")" "${f#./}"
    done )
}

mode="gen"; against=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) mode="check" ;;
    --against) shift; against="${1:?--against needs the canonical repo dir}" ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac; shift
done

new="$(gen_manifest)"

if [ "$mode" = check ]; then
  if [ ! -f "$manifest" ] || ! diff -q <(printf '%s\n' "$new") "$manifest" >/dev/null; then
    echo "models-corpus DRIFT: test/corpus/models/MANIFEST.sha256 is stale." >&2
    echo "Re-sync from canonical harness-wrapper and commit the result." >&2
    exit 1
  fi
  if [ -n "$against" ]; then
    canonical="$against/test/corpus/models/MANIFEST.sha256"
    if [ ! -f "$canonical" ] || ! diff -q "$manifest" "$canonical" >/dev/null; then
      echo "models-corpus CROSS-REPO DRIFT: manifest != canonical $canonical" >&2
      echo "harness-wrapper is canonical; re-vendor its test/corpus/models here." >&2
      exit 1
    fi
    echo "models-corpus manifest OK (in sync with $against)"
    exit 0
  fi
  echo "models-corpus manifest OK"
  exit 0
fi

printf '%s\n' "$new" > "$manifest"
echo "wrote $manifest"

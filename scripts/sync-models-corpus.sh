#!/usr/bin/env bash
# sync-models-corpus.sh — regenerate the models-corpus manifest and optionally
# mirror the corpus into a sibling repo.
#
# The model-picker conformance corpus (test/corpus/models) is vendored
# BYTE-IDENTICALLY into harness-wrapper (canonical) and meta-harness. The two
# repos are "in sync" iff their committed test/corpus/models/MANIFEST.sha256 are
# equal. Each repo's offline conformance test recomputes the hashes and asserts
# the manifest.
#
# Model ids churn faster than the auth signal: when a harness ships a new model,
# re-capture the /model picker and refresh, in this repo, all of
# models.json / bytes.raw / expected.txt / meta.json.expected, then run this
# script to regenerate the manifest and mirror to the sibling checkout.
#
# Usage:
#   scripts/sync-models-corpus.sh            regenerate MANIFEST.sha256 in place
#   scripts/sync-models-corpus.sh --check    verify MANIFEST.sha256 is current (CI); exit 1 on drift
#   scripts/sync-models-corpus.sh --to DIR   regenerate, then mirror corpus+manifest into repo DIR
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

mode="gen"; target=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) mode="check" ;;
    --to) shift; target="${1:?--to needs a repo dir}" ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac; shift
done

new="$(gen_manifest)"
if [ "$mode" = check ]; then
  if [ ! -f "$manifest" ] || ! diff -q <(printf '%s\n' "$new") "$manifest" >/dev/null; then
    echo "models-corpus DRIFT: test/corpus/models/MANIFEST.sha256 is stale." >&2
    echo "Run scripts/sync-models-corpus.sh and commit the result." >&2
    exit 1
  fi
  echo "models-corpus manifest OK"
  exit 0
fi

printf '%s\n' "$new" > "$manifest"
echo "wrote $manifest"
if [ -n "$target" ]; then
  rsync -a --delete "$corpus/" "$target/test/corpus/models/"
  echo "mirrored corpus -> $target/test/corpus/models"
fi

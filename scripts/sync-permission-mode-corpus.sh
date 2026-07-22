#!/usr/bin/env bash
# sync-permission-mode-corpus.sh — regenerate the permission-mode-corpus manifest
# and optionally mirror the corpus into a sibling repo.
#
# The permission-mode conformance corpus (test/corpus/permission-mode) is captured
# HERE — harness-wrapper is canonical for shared corpora (crossrepo/meta-harness/APPLY.md)
# — and mirrored OUT to meta-harness byte-identically. The two repos are "in sync"
# iff their committed test/corpus/permission-mode/MANIFEST.sha256 are equal. Each
# repo's offline conformance test recomputes the hashes and asserts the manifest.
#
# Usage:
#   scripts/sync-permission-mode-corpus.sh            regenerate MANIFEST.sha256 in place
#   scripts/sync-permission-mode-corpus.sh --check    verify MANIFEST.sha256 is current (CI); exit 1 on drift
#   scripts/sync-permission-mode-corpus.sh --to DIR   regenerate, then mirror corpus+manifest into repo DIR
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
corpus="$here/test/corpus/permission-mode"
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
    echo "permission-mode-corpus DRIFT: test/corpus/permission-mode/MANIFEST.sha256 is stale." >&2
    echo "Run scripts/sync-permission-mode-corpus.sh and commit the result." >&2
    exit 1
  fi
  echo "permission-mode-corpus manifest OK"
  exit 0
fi

printf '%s\n' "$new" > "$manifest"
echo "wrote $manifest"
if [ -n "$target" ]; then
  rsync -a --delete "$corpus/" "$target/test/corpus/permission-mode/"
  echo "mirrored corpus -> $target/test/corpus/permission-mode"
fi

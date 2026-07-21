#!/usr/bin/env bash
# check-conformance-corpus.sh — verify meta-harness's vendored copy of the
# cross-language conformance corpus matches this repo's canonical copy.
#
# test/conformance/ is the ONE shared, frozen contract both repos diff their own
# serialization against. harness-wrapper is the canonical side (it regenerates
# the corpus via `make regen-conformance`); meta-harness vendors a byte-for-byte
# copy and only consumes. This script asserts that vendored copy has not drifted.
#
# Direction convention (see scripts/sync-versions.sh's header): a repo's script
# only ever WRITES into its own tree. This script is CHECK-ONLY — it never writes
# into the sibling meta-harness checkout. The pull/vendoring script that copies
# this corpus INTO meta-harness is the consume-side ticket's deliverable and
# lives in meta-harness, not here.
#
# Because the vendored copies are byte-for-byte identical, comparing the two
# MANIFEST.sha256 files is sufficient — the manifest hashes every corpus .json.
#
# This is a dev-machine tool; it is NOT invoked by `make test`.
#
# Usage:
#   scripts/check-conformance-corpus.sh          exit 1 if the vendored copy drifted
#
# The sibling checkout defaults to ~/Work/aether/meta-harness; override with
# META_HARNESS_DIR. The vendored corpus path within it defaults to
# test/conformance; override with META_HARNESS_CORPUS_SUBDIR.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
canonical="$here/test/conformance"
sibling_root="${META_HARNESS_DIR:-$HOME/Work/aether/meta-harness}"
vendored="$sibling_root/${META_HARNESS_CORPUS_SUBDIR:-test/conformance}"

canonical_manifest="$canonical/MANIFEST.sha256"
vendored_manifest="$vendored/MANIFEST.sha256"

if [ ! -f "$canonical_manifest" ]; then
  echo "canonical manifest missing: $canonical_manifest" >&2
  echo "regenerate with: make regen-conformance" >&2
  exit 1
fi

if [ ! -d "$vendored" ] || [ ! -f "$vendored_manifest" ]; then
  echo "meta-harness vendored corpus not found: $vendored_manifest" >&2
  echo "clone meta-harness at \$META_HARNESS_DIR and vendor test/conformance/ there" >&2
  echo "(the pull/vendoring script lives in meta-harness — the consume-side ticket)" >&2
  exit 1
fi

if ! diff -u "$canonical_manifest" "$vendored_manifest"; then
  echo "conformance corpus DRIFT: $vendored_manifest differs from $canonical_manifest" >&2
  echo "The vendored copy in meta-harness is out of sync with this repo's canonical corpus." >&2
  echo "Re-run meta-harness's pull/vendoring script to refresh it, then re-check." >&2
  exit 1
fi

echo "conformance corpus parity OK"

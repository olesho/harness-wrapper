#!/usr/bin/env bash
# Runs the depguard layer rules in .golangci.yml over the tree, then proves
# the rules still bite: depguard stays silent about a rule whose glob matches
# nothing, so a typo would otherwise look like enforcement.
set -euo pipefail

cd "$(dirname "$0")/.."
mod=github.com/olesho/harness-wrapper
probe=zz_layer_probe.go
probe_dirs=()
trap 'for d in "${probe_dirs[@]}"; do rm -f "$d/$probe"; done' EXIT

golangci-lint run ./...

# expect <deny|allow> <dir> <package name in dir> <import path under module>
expect() {
	local want=$1 dir=$2 pkg=$3 imp=$4 out got=allow
	printf 'package %s\n\nimport _ "%s/%s"\n' "$pkg" "$mod" "$imp" >"$dir/$probe"
	probe_dirs+=("$dir")
	out=$(golangci-lint run "./$dir/" 2>&1 || true)
	rm -f "$dir/$probe"
	if grep -q '(depguard)' <<<"$out"; then got=deny; fi
	if [ "$got" != "$want" ]; then
		echo "layer self-test failed: $dir -> $imp: want $want, got $got" >&2
		echo "$out" >&2
		exit 1
	fi
	echo "ok: $dir -> $imp ($want)"
}

expect deny pkg/screen screen internal/harnesscore
expect deny pkg/screen screen pkg/wrapper/trace
expect deny internal/wrapcore/detector detector pkg/harness/claude
expect allow internal/wrapcore/detector detector pkg/harnessname

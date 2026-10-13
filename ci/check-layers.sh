#!/usr/bin/env bash
# Runs only the depguard layer rules in .golangci.yml over the tree, then
# proves the rules still bite: depguard stays silent about a rule whose glob
# matches nothing, so a typo would otherwise look like enforcement.
set -euo pipefail

cd "$(dirname "$0")/.."
mod=github.com/olesho/harness-wrapper
probe=zz_layer_probe.go
probe_dir=
trap '[ -z "$probe_dir" ] || rm -f "$probe_dir/$probe"' EXIT

want=$(cat ci/golangci-lint.version)
have=$(golangci-lint version --short 2>/dev/null || true)
if [ "v${have#v}" != "$want" ]; then
	echo "warning: golangci-lint ${have:-unknown} found, CI uses $want" >&2
fi

lint() { golangci-lint run --enable-only depguard "$@"; }

lint ./...

# expect <deny|allow> <dir> <import path under module>
# A deny must exit 1 with a depguard finding; an allow must exit 0. Anything
# else (config error, typecheck failure, timeout) fails the self-test.
expect() {
	local want=$1 dir=$2 imp=$3 name out status=0
	name=$(go list -f '{{.Name}}' "./$dir/")
	probe_dir=$dir
	printf 'package %s\n\nimport _ "%s/%s"\n' "$name" "$mod" "$imp" >"$dir/$probe"
	out=$(lint "./$dir/" 2>&1) || status=$?
	rm -f "$dir/$probe"
	probe_dir=
	case "$want:$status" in
	allow:0) ;;
	deny:1) grep -q '(depguard)' <<<"$out" || status=99 ;;
	*) status=99 ;;
	esac
	if [ "$status" = 99 ] || { [ "$want" = allow ] && [ "$status" != 0 ]; }; then
		echo "layer self-test failed: $dir -> $imp: want $want" >&2
		echo "$out" >&2
		exit 1
	fi
	echo "ok: $dir -> $imp ($want)"
}

expect deny pkg/screen internal/harnesscore
expect deny internal/wrapcore/detector pkg/screen
expect deny pkg/turns/generic pkg/chat/memstore
expect deny pkg/screen pkg/wrapper/trace
expect deny internal/wrapcore/detector pkg/harness/claude
expect deny internal/wrapcore/detector pkg/adapter
expect allow internal/wrapcore/detector pkg/harnessname

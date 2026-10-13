#!/usr/bin/env bash
# Runs only the depguard layer rules in .golangci.yml over the tree, then
# proves the rules still bite: depguard stays silent about a rule whose glob
# matches nothing, so a typo would otherwise look like enforcement.
#
# Each probe is a throwaway package <layer dir>/zzprobe that nothing imports,
# so it can import any package without creating an import cycle.
set -euo pipefail

cd "$(dirname "$0")/.."
mod=github.com/olesho/harness-wrapper
probe=zzprobe

clean() { find . -type d -name "$probe" -not -path './.git/*' -prune -exec rm -rf {} +; }
clean # a killed earlier run may have left a probe behind
trap clean EXIT
trap 'exit 130' INT TERM HUP

want=$(cat ci/golangci-lint.version)
have=$(golangci-lint version --short 2>/dev/null || true)
if [ "v${have#v}" != "$want" ]; then
	echo "warning: golangci-lint ${have:-unknown} found, CI uses $want" >&2
fi

lint() { golangci-lint run --enable-only depguard "$@"; }

lint ./...

# expect <deny|allow> <layer dir> <import path under module>
# A deny must exit 1 with a depguard finding; an allow must exit 0. Anything
# else (config error, typecheck failure, timeout) fails the self-test.
expect() {
	local want=$1 dir=$2 imp=$3 out status=0
	mkdir "$dir/$probe"
	printf 'package %s\n\nimport _ "%s/%s"\n' "$probe" "$mod" "$imp" >"$dir/$probe/probe.go"
	out=$(lint "./$dir/$probe/" 2>&1) || status=$?
	rm -rf "${dir:?}/$probe"
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

# screen-layer
for imp in internal/wrapcore pkg/wrapper pkg/wrapper/trace pkg/turns \
	internal/chatcore pkg/chat; do
	expect deny pkg/screen "$imp"
done
# wrapper-layer, from both of its globs
for imp in pkg/screen pkg/turns internal/chatcore pkg/chat; do
	expect deny internal/wrapcore "$imp"
	expect deny pkg/wrapper "$imp"
done
# turns-layer
for imp in internal/chatcore pkg/chat pkg/chat/memstore; do
	expect deny pkg/turns "$imp"
done
# core-does-not-import-consumers
for imp in pkg/harness pkg/harness/claude internal/harnesscore pkg/oneshot \
	pkg/turnproto internal/env pkg/adapter pkg/contract pkg/harnessenv; do
	expect deny internal/chatcore "$imp"
done
# neighbours that share a prefix, and packages the core legitimately uses
for imp in pkg/harnessname pkg/discovery/models pkg/transcript pkg/containment; do
	expect allow internal/chatcore "$imp"
done

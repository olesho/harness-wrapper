#!/bin/bash
# guest.sh CELL MODE OUT
#
# Runs inside the virtme-ng guest as root (vng's default for scripts). It
# records the guest environment, gives the unprivileged runner user what a real
# embedder has (a local tmpfs for fixtures and a delegated cgroup subtree), then
# runs the capability probe and the containment suites as that user. Its last
# act writes OUT/guest-status, which the host step checks in addition to vng's
# exit status, so a lost status fails the cell.
#
# MODE is "require" (the kernel must enforce ABI 9: every Landlock skip is a
# failure, the stress run goes inside the domain), "expect-unavailable" (a
# refusal cell: contained launches must be refused, and the probe must find no
# enforceable ABI 9) or "smoke" (the pinned harness binaries named by
# HW_REAL_CLAUDE and HW_REAL_CODEX run their credential-free smoke tests, which
# must both pass).
#
# Environment from the host: KCONFIG (kernel config path), CGO_ENABLED,
# WRAP=seccomp for the seccomp refusal cell, STRESS (<runs>x<spawns>),
# HW_REAL_CLAUDE/HW_REAL_CODEX in smoke mode, PATH/GOCACHE/GOMODCACHE.
set -uo pipefail
cell=$1 mode=$2 out=$3
status=1
trap 'dmesg > "$out/dmesg.txt" 2>&1; echo "$status" > "$out/guest-status"' EXIT

{
	echo "cell=$cell mode=$mode cgo=${CGO_ENABLED:-} wrap=${WRAP:-} stress=${STRESS:-}"
	uname -a
	echo "cmdline=$(cat /proc/cmdline)"
	echo "lsm=$(cat /sys/kernel/security/lsm 2>/dev/null)"
	echo "clocksource=$(cat /sys/devices/system/clocksource/clocksource0/current_clocksource)"
	echo "cgroupfs=$(stat -fc %T /sys/fs/cgroup)"
} >"$out/guest-env.txt"

# Fixtures on a guest-local tmpfs, never on the virtiofs share of the host root.
mount -t tmpfs -o mode=1777,size=4g tmpfs /mnt || exit
# What systemd's Delegate=yes gives a service: a subtree the user may manage.
mkdir -p /sys/fs/cgroup/ci && chown -R runner:runner /sys/fs/cgroup/ci &&
	echo $$ >/sys/fs/cgroup/ci/cgroup.procs || exit

as_runner() {
	setpriv --reuid=runner --regid=runner --init-groups -- \
		env HOME=/home/runner TMPDIR=/mnt XDG_STATE_HOME=/mnt/state "$@"
}
wrap=()
[ "${WRAP:-}" = seccomp ] && wrap=("$out/kernelprobe" -seccomp-deny-landlock --)

# 1. Capability gate. In require mode it fails when ABI 9 cannot be enforced;
#    in expect-unavailable mode it fails when the refusal cell is not one.
probe=(-min-abi 9 -config "$KCONFIG" -json "$out/probe.json")
[ "$mode" = expect-unavailable ] && probe+=(-expect-unavailable)
as_runner "${wrap[@]}" "$out/kernelprobe" "${probe[@]}" >"$out/probe.txt" 2>&1 || exit

# 2. The suites, unprivileged, against the host toolchain and module cache.
pkgs=(./internal/landlock/... ./internal/contain/... ./pkg/wrapper/... ./pkg/chat/... ./pkg/harness/... ./cmd/harness-chatd/...)
envs=(GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local HW_LANDLOCK_CELL="$cell")
run=()
smoke=(TestRealClaudeContained TestRealCodexContained)
case $mode in
expect-unavailable)
	envs+=(HW_LANDLOCK_REQUIRE_ABI=0 HW_LANDLOCK_EXPECT_UNAVAILABLE=1)
	run=(-run 'Refus|Unavailable|Unsupported|Uncontained|Downgrade')
	;;
smoke)
	if [ -z "${HW_REAL_CLAUDE:-}" ] || [ -z "${HW_REAL_CODEX:-}" ]; then
		echo "smoke mode needs HW_REAL_CLAUDE and HW_REAL_CODEX" >"$out/go-test.stderr"
		exit 1
	fi
	pkgs=(./pkg/wrapper/)
	run=(-run '^(TestRealClaudeContained|TestRealCodexContained)$')
	envs+=(HW_LANDLOCK_REQUIRE_ABI=9 HW_LANDLOCK_REQUIRE_SUPERVISION=1
		HW_REAL_CLAUDE="$HW_REAL_CLAUDE" HW_REAL_CODEX="$HW_REAL_CODEX")
	;;
*)
	# Every Landlock or supervision skip is a failure here, and the stress
	# run goes inside the domain.
	envs+=(HW_LANDLOCK_REQUIRE_ABI=9 HW_LANDLOCK_REQUIRE_SUPERVISION=1 HW_CONTAIN_STRESS="${STRESS:-200x320}")
	;;
esac
as_runner "${wrap[@]}" env "${envs[@]}" \
	go test -count=1 -timeout 45m -json "${run[@]}" "${pkgs[@]}" \
	>"$out/go-test.json" 2>"$out/go-test.stderr"
status=$?
# A smoke test that skipped or never ran is a failure, not a pass.
if [ "$mode" = smoke ] && [ "$status" = 0 ]; then
	for t in "${smoke[@]}"; do
		grep -q "\"Action\":\"pass\",\"Package\":\"[^\"]*\",\"Test\":\"$t\"" "$out/go-test.json" || status=1
	done
fi
exit "$status"

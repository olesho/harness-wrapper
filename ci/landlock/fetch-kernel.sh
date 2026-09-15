#!/bin/bash
# fetch-kernel.sh KEY ARCH DEST
#
# Fetches a pinned Ubuntu mainline kernel listed in kernels.json, verifies both
# packages against their pinned SHA-256 on every run (a restored cache is
# re-verified, never trusted), and extracts them into DEST. Extract only onto a
# case-sensitive filesystem: the modules tree holds names that differ only in
# case (xt_TCPMSS.ko / xt_tcpmss.ko).
set -euo pipefail
key=$1 arch=$2 dest=$3
pins=$(dirname "$0")/kernels.json
cache=${KERNEL_DEB_CACHE:-$dest/.debs}

field() { jq -er --arg k "$key" "$1" "$pins"; }
# shellcheck disable=SC2016 # $k is a jq variable, not a shell one
dir=$(field '.[$k].dir')
# shellcheck disable=SC2016
kver=$(field '.[$k].kver')
# shellcheck disable=SC2016
build=$(field '.[$k].build')

mkdir -p "$cache" "$dest"
for pkg in image modules; do
	case $pkg in
	image) name=linux-image-unsigned-$kver-generic_$kver.${build}_$arch.deb ;;
	modules) name=linux-modules-$kver-generic_$kver.${build}_$arch.deb ;;
	esac
	sum=$(jq -er --arg k "$key" --arg a "$arch" --arg p "$pkg" '.[$k][$a][$p]' "$pins")
	[ -f "$cache/$name" ] ||
		curl -fsSL --retry 3 -o "$cache/$name" "https://kernel.ubuntu.com/mainline/$dir/$arch/$name"
	echo "$sum  $cache/$name" | sha256sum -c -
	dpkg -x "$cache/$name" "$dest"
done
ls "$dest"/boot/vmlinuz-"$kver"-generic "$dest"/boot/config-"$kver"-generic

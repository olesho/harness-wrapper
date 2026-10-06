#!/bin/sh
# Fetch the pi release the probe runs, for this platform:
#
#   probes/pirpc/fetch.sh DIR [VERSION]
#
# leaves DIR/pi: the release's files, DIR/pi/pi its executable, at VERSION
# (default: pi's pin in pkg/versions/versions.json), the tarball checked
# against the release's SHA256SUMS. Then:
#
#   HW_REAL_PI=DIR/pi/pi go test -count=1 -v ./probes/pirpc/
set -eu

dir=${1:?usage: fetch.sh DIR [VERSION]}
root=$(cd "$(dirname "$0")/../.." && pwd)
version=${2:-$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["pi"]["pinned"])' "$root/pkg/versions/versions.json")}
mkdir -p "$dir"
dir=$(cd "$dir" && pwd)

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) platform=darwin-arm64 ;;
  Darwin-x86_64) platform=darwin-x64 ;;
  Linux-aarch64 | Linux-arm64) platform=linux-arm64 ;;
  Linux-x86_64) platform=linux-x64 ;;
  *) echo "fetch.sh: no pi release for $(uname -s) $(uname -m)" >&2; exit 2 ;;
esac

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

release=https://github.com/earendil-works/pi/releases/download/v$version
echo "fetching pi $version ($platform)" >&2
curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" "$release/SHA256SUMS"
curl -fsSL --retry 3 -o "$tmp/pi.tar.gz" "$release/pi-$platform.tar.gz"
want=$(awk -v f="pi-$platform.tar.gz" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
got=$(sha256 "$tmp/pi.tar.gz")
[ -n "$want" ] && [ "$got" = "$want" ] || { echo "fetch.sh: pi $version $platform: sha256 $got, SHA256SUMS says '$want'" >&2; exit 1; }
rm -rf "$dir/pi"
tar -xzf "$tmp/pi.tar.gz" -C "$dir"
got=$("$dir/pi/pi" --version)
[ "$got" = "$version" ] || { echo "fetch.sh: $dir/pi/pi --version says $got, want $version" >&2; exit 1; }
echo "$dir/pi/pi"

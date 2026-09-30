#!/bin/sh
# Fetch the pinned harness binaries the probe runs, for this platform:
#
#   probes/saveload/fetch.sh DIR
#
# leaves DIR/claude and DIR/codex — the native codex, not its node shim — at
# the versions pkg/versions/versions.json pins, each checked against what its
# publisher lists for it: claude's release manifest, and the npm registry's
# integrity of codex's package. Then:
#
#   HW_REAL_CLAUDE=DIR/claude HW_REAL_CODEX=DIR/codex go test -count=1 -v ./probes/saveload/
set -eu

dir=${1:?usage: fetch.sh DIR}
root=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$dir"
dir=$(cd "$dir" && pwd)

pinned() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]]["pinned"])' "$root/pkg/versions/versions.json" "$1"
}

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) platform=darwin-arm64 triple=aarch64-apple-darwin ;;
  Darwin-x86_64) platform=darwin-x64 triple=x86_64-apple-darwin ;;
  Linux-aarch64 | Linux-arm64) platform=linux-arm64 triple=aarch64-unknown-linux-musl ;;
  Linux-x86_64) platform=linux-x64 triple=x86_64-unknown-linux-musl ;;
  *) echo "fetch.sh: no pinned binaries for $(uname -s) $(uname -m)" >&2; exit 2 ;;
esac

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# claude: the binary itself, and its checksum in the release's manifest.
claude=$(pinned claude-code)
releases=https://downloads.claude.ai/claude-code-releases/$claude
curl -fsSL --retry 3 -o "$tmp/manifest.json" "$releases/manifest.json"
want=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["platforms"][sys.argv[2]]["checksum"])' "$tmp/manifest.json" "$platform")
echo "fetching claude $claude ($platform)" >&2
curl -fsSL --retry 3 -o "$tmp/claude" "$releases/$platform/claude"
got=$(sha256 "$tmp/claude")
[ "$got" = "$want" ] || { echo "fetch.sh: claude $claude $platform: sha256 $got, the manifest says $want" >&2; exit 1; }
chmod +x "$tmp/claude"
mv "$tmp/claude" "$dir/claude"

# codex: the platform's npm package, and its integrity in the registry.
codex=$(pinned codex)
curl -fsSL --retry 3 -o "$tmp/codex.json" "https://registry.npmjs.org/@openai/codex/$codex-$platform"
tarball=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dist"]["tarball"])' "$tmp/codex.json")
want=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dist"]["integrity"])' "$tmp/codex.json")
echo "fetching codex $codex ($platform)" >&2
curl -fsSL --retry 3 -o "$tmp/codex.tgz" "$tarball"
got=sha512-$(openssl dgst -sha512 -binary "$tmp/codex.tgz" | openssl base64 -A)
[ "$got" = "$want" ] || { echo "fetch.sh: codex $codex $platform: integrity $got, the registry says $want" >&2; exit 1; }
tar -xzf "$tmp/codex.tgz" -C "$tmp" "package/vendor/$triple/bin/codex"
chmod +x "$tmp/package/vendor/$triple/bin/codex"
mv "$tmp/package/vendor/$triple/bin/codex" "$dir/codex"

echo "claude $("$dir/claude" --version) sha256 $(sha256 "$dir/claude")"
echo "codex  $("$dir/codex" --version) sha256 $(sha256 "$dir/codex")"

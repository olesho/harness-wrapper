#!/usr/bin/env sh
# Managed by veracity — a thin shim. Do NOT add logic here; the veracity binary
# owns all behavior. Arg 1 is the host agent (claude|codex).
root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$root" || exit 0
exec veracity hook stop --agent "${1:-claude}"

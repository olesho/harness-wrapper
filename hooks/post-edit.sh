#!/usr/bin/env sh
# Managed by harness — a thin shim. Do NOT add logic here; the harness binary
# owns all behavior. Arg 1 is the host agent (claude|codex).
root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$root" || exit 0
exec harness hook post-edit --agent "${1:-claude}"

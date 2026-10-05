# ADR-016: an MCP connector's secret header comes from a file

**Status:** Accepted (2026-10-05)

**Intent:** principle 3, *normalize, don't leak*, and principle 6, *evolve public contracts
deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 3, how a harness
reads a connector's header stays in its profile; the host names a file, the same for every
harness. Under principle 6, the field is part of interface 1.3, beside the one it supersedes.

## Context

An http connector's secret header — an MCP server's `Authorization`, say — reaches the harness
through `HeadersEnv`: the variable its value is read from, so the value stays out of the rendered
configuration. The host has to put the value in that variable, in the environment it starts the
adapter in. That environment is the harness's and every process the harness starts — its tools,
its stdio MCP servers, a shell a prompt asked for — and `adapter.HostEnv` says it carries no
credential. agentd did it anyway, through `HW_HARNESS_ENV`, for want of another way.

Both harnesses can do better, to a degree:

- claude runs an http server's `headersHelper`, a command whose JSON output is the headers, each
  time it connects. It runs it only in a workspace whose trust is persisted, which the profile
  writes already.
- codex reads a header from its configuration (`http_headers`) or its environment
  (`env_http_headers`). A later codex has `http_headers_helper`; the pinned 0.144.5 has not.

## Decision

1. **Interface 1.3 adds `HTTPConnector.HeadersFile`:** a header, mapped to the absolute path of a
   file holding its value. The harness reads it when it connects, where it can, so the value is in
   no rendered configuration and no process's environment; where it cannot, the adapter reads the
   file when it starts the harness and gives the value to that process alone. The host may rewrite
   a file between launches. `HeadersEnv` stays, superseded.
2. **`CheckSpec` checks an http connector's headers:** each a token, named once across `headers`,
   `headers_env` and `headers_file` in any case, and each file an absolute, clean path.
3. **The claude profile** writes a script per server beside `mcp.json`, holding the files' paths
   and no value, and names it the server's `headersHelper`. The script prints each value read from
   its file, its newlines dropped and its backslashes, quotes and tabs escaped, and fails when a
   file cannot be read.
4. **The codex profile** names a variable for each such header in `env_http_headers`, and Start
   reads each file into that variable, in codex's environment alone.

## Consequences

- With claude, a connector header's value is on disk once, in the host's file, and in claude's
  memory while it connects; not in its environment, its children's or its configuration.
- With the pinned codex, the value is in codex's environment and its children's, as with
  `HeadersEnv`, but no longer in the host's environment. A codex pin with `http_headers_helper`
  can read the file itself.
- A host that rewrites a file reaches a claude that connects again and a codex that starts again.

## Follow-ups

- When the codex pin moves to a version with `http_headers_helper`, render a helper as for claude.

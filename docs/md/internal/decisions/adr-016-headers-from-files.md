# ADR-016: an MCP connector's secret header comes from a file

**Status:** Accepted (2026-10-05); amended 2026-10-05

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
   and no value, and makes `/bin/sh` running it the server's `headersHelper`, which claude hands
   to a shell: no provisioned file is executable. The script prints each value read from its file,
   its newlines dropped and its backslashes, quotes and tabs escaped, and fails when a file cannot
   be read.
4. **The codex profile** writes the same script per server and makes `/bin/sh` running it the
   server's `http_headers_helper`, which codex 0.160 runs each time it connects. (Before codex
   0.160 it named a variable for each such header in `env_http_headers`, and Start read each file
   into that variable, in codex's environment alone.)
5. **The conformance scenario `headers`** provisions an http connector whose header comes from a
   file, applies the result as a Supervisor does, and checks that the value is in no rendered file;
   where the fixture's harness connects to MCP servers (`Fixture.MCP`), that the kit's MCP server
   hears it.

## Consequences

- With either harness, a connector header's value is on disk once, in the host's file, and in the
  harness's memory while it connects; not in its environment, its children's or its
  configuration.
- A host that rewrites a file reaches a harness that connects again.

## History

- 2026-10-05 — amended: codex 0.160 runs `http_headers_helper`, so the codex profile renders a
  helper as for claude, and no header's value goes through codex's environment
  ([ADR-019](adr-019-pin-codex-0-160.md)).

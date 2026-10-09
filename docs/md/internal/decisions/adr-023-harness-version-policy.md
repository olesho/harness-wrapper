# ADR-023: A harness version policy

**Status:** Accepted (2026-10-09)

**Intent:** principle 6, *evolve public contracts deliberately*, and principle 7, *say what is
enforced, not what is intended* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 6,
how strictly a Session's harness is held to the pin is a field of interface 1.7, its default the
behaviour every profile but one already had, and a request of an earlier minor cannot carry it. Under
principle 7, the policy says exactly what is checked: under `strict` the version, under either policy
the capabilities each profile relies on, and every Session reports the version it runs. It also
applies 3 (how a harness's version is learned is each profile's to know; the policy is one word to
the runtime).

## Context

Every profile pins a harness version (`versions.json`, `Descriptor.harness.version`): the one its
conformance has passed with, and whose record, protocol and quirks it was written against. The pin
was enforced unevenly:

- `claude-code-tui` refused any claude but the pin (`version_unsupported`), from its first phase: it
  parses claude's debug log, a format claude does not document.
- `claude-code`, `codex` and `pi` ran whatever binary the distribution held. What they check is
  capability, not version: claude's `system/init` must advertise `interrupt_receipt_v1` and
  `msg_lifecycle_v1`; codex must answer `initialize` and open its thread; pi must name the session in
  `get_state` and load the tag extension.
- No Session said which version it ran, so a runtime could not tell a pinned Session from one on
  whatever was installed.

agentd's bundles ship the pin as the harness distribution, so there the question is moot. It is not
for a runtime that installs the harness itself, a developer's machine with a newer claude on `PATH`,
or a pin move rolled out unevenly: there one profile refused what the other three ran, and nothing
reported either. The TUI profile's documentation already listed a *flexible* mode — the capability
checks alone — as planned beside its strict one.

## Decision

1. **Interface 1.7 adds `AgentSpec.version_policy`**: `strict` or `flexible`, a typed string
   (`contract.VersionPolicy`) with its set in the schema. It is per agent and harness-neutral, beside
   the spec's other launch choices (`model`, `effort`, `permission_posture`), and rendered by
   `Provision` into each profile's `open_config`, which every `Open` starts from.
   - **strict:** `Open` refuses a harness binary whose version is not the pin, or cannot be learned,
     with `open_failed`/`version_unsupported` naming both versions.
   - **flexible:** no version is checked. Every capability or protocol check a profile has stays,
     failing with the reason it always did.
2. **Empty is flexible.** Three of the four profiles behave exactly as before, and their
   `open_config` is byte-identical for a spec without a policy (`omitempty`).
3. **Interface 1.7 adds `OpenResult.harness_version`**: the version the Session's harness reports,
   under either policy, empty when the adapter could not learn it. It is the open's result because
   the version is a fact of the process the open started, fixed for its life; `State` changes, and the
   Descriptor names the pin, not what runs.
4. **Both are part of 1.7, behind no capability**: every 1.7 adapter honours them. `CheckSpec` refuses
   a policy outside the set (`invalid_spec`, `version_policy`) and a policy for an adapter that
   declares an earlier minor (`unsupported`); `contract.CheckMinor` refuses a request of an earlier
   minor that carries one (`protocol`, `spec.version_policy`), since that minor's adapter would drop
   the field unread and run flexible where strict was asked.
5. **Each profile learns its version as cheaply as it can**, before or as the harness starts:
   claude from `claude --version` (about 0.2 s; claude's `initialize` answer carries no version, and
   `system/init`, which does, comes only with the first turn); codex from the user agent its
   `initialize` answer already carries, so its app-server starts and is stopped, before any thread
   opens, when strict refuses it; pi from the `package.json` beside its executable, which pi reads
   its own version from. A shared `adapter.CheckHarnessVersion` applies the policy.
6. **`claude-code-tui`'s pin refusal becomes the policy.** Under `flexible`, its default now, a claude
   other than the pin opens, and the debug log's `[engine]` gate decides whether the profile can read
   it, as before.
7. **The kit's `version-policy` scenario** holds the rules for an adapter of 1.7 or later, with a
   fixture's `NonPin` making the harness another version; the fake adapter breaks each rule.

## Alternatives

- **An environment variable on the Host** (`HW_VERSION_POLICY`): no contract change, but invisible to
  the Supervisor, which provisions the agent and journals its spec; a Host started without it would
  silently change policy, and the policy could not differ per agent.
- **A key in each profile's `open_config` only**: the `open_config` is opaque to the runtime, so it
  would have no harness-neutral way to ask for it, and each profile would invent its own name for
  the same choice.
- **Strict by default**: what the TUI profile did. It would make `claude-code`, `codex` and `pi`
  refuse binaries they run today, breaking every runtime that installs its own harness, for a check
  their capability gates already make where it matters. A runtime that wants the pin alone says so.
- **A capability (`version_policy`)**: a runtime would have to check for it before every spec it
  writes, for a field every adapter can honour at no cost. The minor carries it instead.
- **Reporting the version in `State` or the Descriptor**: `State` is a changing snapshot and the
  Descriptor is pure, describing the pin; the running version is the open's.

## Boundary

Guaranteed, for an adapter of 1.7 or later:

- Under `strict`, no Session runs a harness that did not report the pin.
- Under either policy, every check a profile made of what it relies on still holds.
- `harness_version` is what the harness itself reported (its `--version`, its user agent, its
  release's metadata), never the pin assumed.

Not guaranteed:

- That a harness other than the pin behaves as the profile expects beyond its capability checks. A
  claude whose debug log words its `[engine]` lines otherwise, or a codex whose rollout changed, opens
  under `flexible` and may fail later, by turn, not at `Open`.
- That `harness_version` is the binary's: claude's and codex's are what the process says; pi's is
  what the release's `package.json` says, which pi itself reads.
- A version on a request of a minor before 1.7, which `CheckMinor` refuses rather than honours.

## Evidence

- `TestClaudeVersionPolicy` and `TestClaudeTUIVersionPolicy`, with `HW_REAL_CLAUDE` the pinned claude
  2.1.283 and `HW_NONPIN_CLAUDE` an installed 2.1.285 (macOS arm64, mock API): both profiles open
  2.1.285 under the default and `flexible`, reporting `2.1.285`, and refuse it under `strict` with
  `version_unsupported: claude 2.1.285 is not the pinned claude 2.1.283`.
- codex 0.160.0's `initialize` answer: `"userAgent":"harness-wrapper/0.160.0 (Mac OS …) …"`.
- claude 2.1.283's `initialize` control response carries no version.

## Consequences

- A runtime that relied on `claude-code-tui` refusing a non-pin claude now gets it run, unless it asks
  for `strict`. agentd ships the pin and is unaffected in practice; it sets `strict` where it wants
  the refusal kept.
- Each `claude-code` open runs `claude --version` first.
- A runtime can now see which version every Session ran, and journal it.

## Follow-ups

- A Descriptor field naming the versions a profile has passed beyond the pin, if a runtime needs
  more than strict or flexible.

# Harness Adapter Interface

`pkg/contract` is the Go interface a runtime uses to drive a harness without naming it: describe it,
render its configuration, open and reopen its sessions, send, interrupt, answer, observe with
acknowledgement, and read its record after a crash ([ADR-012](decisions/adr-012-harness-adapter-interface.md)).
The specification is
[Harness Adapter Interface v1](https://coplan.olehluchkiv.com/d/engine-contract-v1-specification); this
package is its normative form, contract version `harness-adapter/1.0`.

## The packages

| Package | What it is |
|---|---|
| `pkg/contract` | The types, the `Adapter` / `Session` / `Record` interfaces, errors, bounds, the registry (`Register`, `Lookup`) and the generated JSON Schema (`schema.json`, embedded as `contract.Schema`). Standard library only, so importing it links nothing else of hw (`TestStandardLibraryOnly`). |
| `pkg/contract/conformance` | The conformance kit: a fake Agent Adapter — a Supervisor that applies what `Provision` renders, and a Host that observes, commits and acknowledges — plus scenarios. |
| `pkg/contract/fakeadapter` | An adapter for a harness that lives in the process: the kit's reference, and a stand-in for a runtime's own tests. |

## Two callers

- The **Supervisor** calls `Describe` and `Provision`. `Provision` is pure: it turns a harness-neutral
  `AgentSpec` into files, an opaque `open_config` and the paths archives include and skip. The
  Supervisor writes the files — beneath their roots, never through a symlink, with modes no wider
  than `0644` (`ProvisionResult.Validate`, `conformance.Apply`).
- The **Host** opens Sessions (`NewSession`, then `Open`) and record handles (`OpenRecord`), feeds a
  Session one input at a time, and acknowledges each batch of observations only after the
  Supervisor has committed it.

A profile registers its adapter under its harness's name in `init`; a runtime links the harnesses it
offers through one file of blank imports and finds them with `contract.Lookup`.

## harness-wrapper's Harness Adapter

`pkg/adapter` is hw's implementation of the interface: one adapter, with a **profile** per harness.
It is the shared part, and names no harness:

- **Sessions.** The states, one outstanding `Send`, the admission gate (a usage, auth or billing
  error closes it; a usage wall with a known reset opens it again then, and nothing is retried),
  an `Interrupt` that names its input and is ordered after an outstanding send, `Answer`, and
  `Close` with `stopped` and `drained`. An uncertain send keeps the Session busy until its turn ends.
- **Observe and Ack.** One cursor over the live events and the record, in the order learned. A
  record chunk may span batches: its reset, rescan and faults go with its first item, its
  checkpoint with its last, and the reader moves past it only once that batch is acknowledged.
- **Submission markers**, one per input under `layout.scratch/markers`, synced before the harness
  gets the input. `OpenRecord` and `Recover` rest on them: only an intact store's missing marker
  proves an input never ran.

A profile (`adapter.Profile`) supplies what is its harness's own: the Descriptor, `Provision`, a
`Transport` to the running harness (submit, interrupt, answer, stop, and its events) and a
`Reader` of its record (chunks of record-origin observations, commit, and the evidence for
`Recover`). It registers with `adapter.Register` under its harness's name.

## The Claude Code profile

`pkg/adapter/claudecode` registers `claude-code`. Its harness distribution, under `harness_root`, is
the pinned claude (`bin/claude`) and the profile's hook helper, `cmd/claude-code-hook`
(`bin/claude-code-hook`).

- **Provision** renders what agentd rendered before it (`TestProvisionMatchesAgentdProfile` holds the
  two side by side): `settings.json` with the hooks, `.claude.json` with onboarding, bypass and
  workspace trust answered, the persona, skills, memory, `mcp.json` and the workspace's `CLAUDE.md`,
  and `open_config` with claude's arguments and environment. Each hook runs the helper, which writes
  what it reports to the spool: the scratch root itself, beside the markers' directory, so a spool a
  host kept before this profile, at the root it now names scratch, is read where it is.
- **Transport:** stream-json, one claude process per Session in a process group of its own. An input
  is a user message whose uuid is the input's native id, a fresh UUID kept in its submission marker;
  claude's `command_lifecycle` receipt returns `Send`, and its transcript keeps the uuid as the prompt
  entry's. A turn ends with claude's `result`: by `is_error` and `terminal_reason`. `cancelled` needs
  claude's word that the message never started — claude says `cancelled` after the result of a turn it
  interrupted or failed, too. A failed turn is classed by the synthetic message's tag, the HTTP status
  and, for a 429, whether the account refused it: a usage wall (`You've hit your … limit · resets …`)
  closes the gate until its reset; the server's 429 (`not your usage limit`) is an `api` error.
- **Record:** the session transcript, followed from the checkpoint, and the hook spool. Checkpoint
  format 1 is the transcript follower's checkpoint — the one agentd stored — so stored checkpoints
  resume where they stood (`TestNodeDBCheckpoint`). Entries become `user_input`, `assistant_text`,
  `tool_use`, `tool_result` and `api_error`, keyed by the entry's uuid (and block) or the tool use id,
  with `entry` set to the entry's uuid. A turn's end is in the record as its final assistant entry
  (`stop_reason: end_turn`), a synthetic API-error entry or an interrupt entry, each a record-origin
  `turn_ended`. Spool files become `tool_started`, `tool_finished` and the subagents' start and stop;
  a file is deleted once its chunk is acknowledged, or at once when it reports nothing.
- **Recover** finds the prompt entry by the marker's native id, then that evidence: without either,
  `unknown`.
- **`LegacyHookFacts`**, off by default, makes each Stop and SessionEnd hook's firing an observation
  of kind `x_legacy_hook`, outside the contract: a transitional source for a host that must go on
  publishing the advisory facts it published before (agentd's `hook.stop` and `hook.session_end`).

`TestClaudeConforms` runs the conformance kit, and `TestClaudeObservations` the profile's own
checks, against a real claude driving `internal/mockapi` — a Go port of agentd's P11 mock Messages
API — when `HW_REAL_CLAUDE` names the pinned binary. The `harness-adapter` workflow runs them on
Linux with the pinned claude it downloads and verifies.

## Running the kit

```go
func TestConformance(t *testing.T) {
	conformance.Run(conformance.Testing(t), conformance.Fixture{
		Adapter:     myAdapter,
		HarnessRoot: root,
		Spec:        contract.AgentSpec{PermissionPosture: contract.PostureBypass},
		Credential:  stageCredential, // nil for a harness that needs none
		Provisioned: pointAtMockAPI,  // e.g. ANTHROPIC_BASE_URL for P11's mockapi.py
		Kill:        killHarness,     // crash the harness process without Close
		HideBinary:  hideBinary,      // optional
	})
}
```

The scenarios speak the prompt language of agentd's P11 mock Messages API — `PING`, `SLOW`,
`STALL`, `TOOL`, `ERR <code> <k>`, `BIG`, `ASK`, and `LIMIT` for a usage wall (a 429 the account's
limit refused, which a harness does not retry; `ERR 429 <k>` is the server's load, which it does) —
so a real harness runs them against such a mock.
Each check names its rule (`[interrupt.other-turn] …`), and `TestBrokenAdaptersFail` breaks each rule
in the fake adapter (`fakeadapter.Options.Break`) and requires the kit to fail exactly that rule.
A scenario a harness cannot run is named in `Fixture.Skip`, with the reason.

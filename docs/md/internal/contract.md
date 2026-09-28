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
`STALL`, `TOOL`, `ERR <code> <k>`, `BIG`, `ASK` — so a real harness runs them against that mock.
Each check names its rule (`[interrupt.other-turn] …`), and `TestBrokenAdaptersFail` breaks each rule
in the fake adapter (`fakeadapter.Options.Break`) and requires the kit to fail exactly that rule.
A scenario a harness cannot run is named in `Fixture.Skip`, with the reason.

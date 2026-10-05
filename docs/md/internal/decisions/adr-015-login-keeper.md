# ADR-015: a runtime keeps a subscription login, and lends it behind its broker

**Status:** Accepted (2026-10-04)

**Intent:** principle 3, *normalize, don't leak*, principle 6, *evolve public contracts
deliberately*, and principle 7, *say what is enforced, not what is intended*
([INTENT](../../../../INTENT.md#design-principles)). Under principle 3, how a harness signs in,
where it keeps its login and how it refreshes it stay in its profile; the runtime keeps a login
through one vocabulary. Under principle 6, the keeper is part of interface 1.2, behind its own
capability. Under principle 7, the keeper lends a credential with nothing that refreshes it, and
says when the login can no longer be refreshed, rather than lending what no longer works.

## Context

[ADR-014](adr-014-brokered-credentials.md) keeps a credential out of the agent: the agent works
with a placeholder, and the runtime's broker swaps in the credential. That suits a credential that
lasts: an API key, claude's `setup-token`. A ChatGPT login does not last. Its access token expires,
and the refresh token that renews it rotates on every use: a copy that refreshes logs out every
other holder, and agentd's probe found that a broker cannot hold one either (its findings 7 and 8).
`codex_chatgpt_login` so far lends a login's access token alone, which works until it expires.

agentd's probe (`probes/ironproxy`, finding 20) proved another way: a trusted user on the runtime,
outside the agents' fence, runs the official codex with its own `CODEX_HOME` and a file credential
store, signed in once with a device code; codex refreshes the login itself, and the runtime hands
each new access token to its broker. That is a separate ChatGPT session; the person's own logins are
untouched. The pinned codex's app-server already offers both halves over the protocol a Session
speaks: `account/login/start` with `chatgptDeviceCode` answers with a URL and a code, and
`account/login/completed` says when the person approved; `account/read` with `refreshToken` has
codex refresh the login on the spot, with no model call.

## Decision

1. **Interface 1.2 lets a runtime keep a login** (capability `login_keeper`).
   - `Descriptor.keeper` names the credential kind a kept login is lent as: one the harness takes,
     and one its egress routes. A kept login is lent behind a broker, never into a workload's
     files (`contract.CheckKeeper`).
   - `Adapter.Keep` opens a keeper over a home of the runtime's keeper identity, which no agent can
     reach. It starts nothing until a method needs the harness.
   - A `Keeper` signs in with a device code (`SignIn`: where to go and the code to enter), reports
     its state (`Status`: signed out, signing in, signed in, expired — with the account, the plan,
     when the credential expires, and why a sign-in or refresh failed), has the harness's own
     client refresh the login (`Refresh`), lends its credential with nothing that refreshes it
     (`Lend`), and signs out.
   - `Lent` is a secret and prints without its credential.
2. **The runtime refreshes before expiry**, by asking: `Refresh` runs the official client's own
   refresh, and the next `Lend` lends what it got. A refused refresh is an error, and `Status` says
   why; once the credential expires the login is `expired` until someone signs in again.
3. **The Codex profile** keeps a ChatGPT login with the pinned codex: `CODEX_HOME` is the keeper's
   home, codex's credential store a file there, and its app-server runs only for a sign-in, a
   refresh and a sign-out. `Lend` reads `auth.json` and lends it without its refresh token: a
   `codex_chatgpt_login`, which `Placeholder` takes.

## Alternatives

- **The runtime drives `codex login --device-auth`** and parses what it prints. It is the probe's
  recipe, and a terminal's text; the app-server says the same in JSON, over the protocol hw already
  speaks to codex.
- **Refresh by running a turn**, as the probe forced it. It spends a model call each time, and
  codex refreshes only once the token has expired; `account/read` refreshes on asking, before.
- **Refresh in the runtime**, from codex's refresh token. It reimplements OpenAI's refresh, which
  rotates and detects reuse, outside the client OpenAI supports.
- **Lend the whole login**, refresh token included, as a file in the workload. The agent could then
  spend it, log the keeper out, or carry it away.
- **Codex's external auth mode** for the agent, fed tokens over the protocol. It changes how every
  agent's codex is opened, and a refresh request it raises would need answering in the agent's
  Host; the placeholder login of ADR-014 needs neither.

## Boundary

Guaranteed: a keeper's lent credential carries nothing that refreshes it; the refresh token stays
in the keeper's home; a sign-in completes only by the person's approval; a login whose refresh was
refused says so (the conformance kit's `keeper` scenario, with a keeper whose sign-in a fixture
approves).

Not guaranteed: that the service keeps refreshing a login. A revoked session, a changed password or
a plan change ends it, and the person signs in again. Nor anything of the agent's own: the keeper
lends, and the runtime's broker and fence keep the credential from the agent (ADR-014).

## Evidence

- agentd's `probes/ironproxy` finding 20: the official codex as a keeper, signed in with a device
  code approved on a phone, refreshing itself across two forced expiries, its access token fed to
  the broker for a fenced codex on placeholders; the person's laptop login unaffected.
- The pinned codex 0.144.5's app-server schema: `account/login/start` takes `chatgptDeviceCode` and
  answers `loginId`, `userCode` and `verificationUrl`; `account/read` takes `refreshToken`.
- `TestKeeperConforms` and the keeper tests, against a fake app-server; `TestCodexKeeperLive`, for a
  person to run with `HW_REAL_CODEX` and `HW_KEEPER_LIVE=1`, signs in, refreshes and signs out
  against ChatGPT.

## Consequences

- An adapter implements `Keep`; without the capability it answers `unsupported`.
- The keeper's app-server needs the network the login lives on (`auth.openai.com`, `chatgpt.com`):
  it runs outside the agents' fence, as the runtime's own identity.
- A codex upgrade that changes the device-code flow, the refresh or `auth.json` changes the keeper,
  verified live before the pin moves.

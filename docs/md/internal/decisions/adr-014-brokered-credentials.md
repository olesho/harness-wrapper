# ADR-014: a harness works with placeholders behind an egress broker

**Status:** Accepted (2026-10-04)

**Intent:** principle 3, *normalize, don't leak*, principle 6, *evolve public contracts
deliberately*, and principle 7, *say what is enforced, not what is intended*
([INTENT](../../../../INTENT.md#design-principles)). Under principle 3, where a harness presents a
credential and what may stand in for it stay in its profile; the runtime that keeps the credential
names neither. Under principle 6, all of it is a minor version: a Descriptor field, one capability
and one Supervisor operation, which a 1.1 caller never meets. Under principle 7, the adapter says
where a credential goes and renders its placeholder; the secrecy is the runtime's broker and fence,
and the interface claims no more.

## Context

Every credential an agent holds today is a file its own workload identity can read: the staged
credential, and the environment or `auth.json` the profile makes of it. Whatever runs as the agent —
a prompt-injected command, a dependency's install script — can copy it.

agentd's plan
[a credential boundary around agents](https://coplan.olehluchkiv.com/d/agentd-a-credential-boundary-around-agen)
keeps the credential out of the workload's reach: the runtime stages a placeholder in its place,
fences the workload's network so that an egress broker on the runtime is its only way out, and the
broker replaces the placeholder with the credential in the requests sent where the credential
belongs. agentd's probe of that design (`probes/ironproxy`, iron-proxy as the broker) ran the pinned
claude and codex that way, against mocks and the live services:

- claude takes a `setup-token`-shaped placeholder in `CLAUDE_CODE_OAUTH_TOKEN`, and with its
  nonessential traffic off reaches `api.anthropic.com` alone, presenting the token in
  `Authorization`;
- codex takes a ChatGPT login whose id and access tokens are unsigned, JWT-shaped placeholders
  carrying the login's plan claims; it sends its model traffic to `chatgpt.com`, over a WebSocket,
  whatever `chatgpt_base_url` says, and an API key to `api.openai.com`;
- codex trusts the broker's certificate authority only through `CODEX_CA_CERTIFICATE`.

Each of those is a fact about a harness, which agentd may not name (its ADR 0004): which hosts a
harness reaches, where it presents each credential kind, what a placeholder must look like for the
harness to take it, and where it reads the certificates to trust.

## Decision

1. **Interface 1.2 keeps credentials behind a broker** (capability `brokered_credentials`).
   - `Descriptor.egress` names the hosts every Session of the harness reaches, and a route per
     credential kind it takes as a placeholder: the exact hosts it presents the credential to and
     the request headers that carry it there. `contract.CheckEgress` holds the form: exact
     lower-case names, kinds the harness takes, each routed once.
   - `Adapter.Placeholder` renders what stands in for a credential: the file the Supervisor stages
     in its place, which Open reads as it reads the credential, and the swaps — each placeholder,
     its secret and its route — the Supervisor hands its broker. It is pure: a function of the
     credential and a nonce of the Supervisor's.
   - The result holds the credential's secrets. It crosses no process boundary in agentd; the
     Supervisor keeps it in memory, gives the secrets to the broker alone, and never journals or
     logs it. `Swap` prints without its secret.
   - `PlaceholderResult.Validate` holds what a broker needs: each swap's hosts and headers within
     the kind's route, placeholders long enough to find, distinct and printable, and no secret in the
     file or in a placeholder.
2. **The runtime passes the broker and its certificates in the environment** it gives every harness:
   `HTTPS_PROXY`, and the certificates to trust in `SSL_CERT_FILE`, through `HW_HARNESS_ENV`. A
   profile hands those certificates to its harness however that harness reads them; the Codex
   profile copies `SSL_CERT_FILE` to `CODEX_CA_CERTIFICATE`.
3. **The Claude Code profile** routes `claude_oauth_token` to `api.anthropic.com` in
   `Authorization`, the one host claude reaches, and renders a token of the same shape: its prefix
   kept, the rest drawn from the nonce (`adapter.TokenPlaceholder`).
4. **The Codex profile** routes `openai_api_key` to `api.openai.com` and `codex_chatgpt_login` to
   `chatgpt.com`, both in `Authorization`. An API key's placeholder is a key of its shape. A
   ChatGPT login's is a login built afresh: JWT-shaped id and access tokens with the login's plan
   and account claims and no other claim, expiring in 2100 so codex never tries to refresh them,
   the login's account id, and the refresh token that refreshes nothing. Its access token is the
   one swap. A login that holds a refresh token has no placeholder, as it has no lent copy.
   `codex_access_token`, never tried behind a broker, has no route.

## Alternatives

- **The runtime names each harness's hosts and placeholder shapes itself.** Fewest hw changes, and
  agentd would hold claude's token prefix, codex's `auth.json` format and its CA variable: the
  harness knowledge ADR-012 moved out of it.
- **Placeholders rendered by `Provision`.** Its result is journaled and replayed, and a
  placeholder's swaps carry the secret. A separate operation keeps secrets out of anything durable.
- **Any random string as the placeholder.** A harness may tell credentials apart by their prefix,
  and codex parses its tokens as JWTs. A placeholder of the credential's shape is taken as the
  credential.
- **A broker that refreshes OAuth logins.** iron-proxy's own refresh cannot keep a rotating refresh
  token (agentd's findings 7 and 8). Refresh stays with whoever holds the refresh token, outside the
  harness; the placeholder login expires in 2100 so the harness never tries.
- **Codex's external auth mode** (`account/login/start` with `chatgptAuthTokens`), which needs no
  `auth.json`. It would change how every ChatGPT login is opened; the placeholder login was proved
  with the file codex already reads.

## Boundary

Guaranteed: an adapter that declares `brokered_credentials` names a route for every kind it renders
a placeholder for, renders the same placeholder for the same credential and nonce and another for
another nonce, keeps every secret out of the placeholder file, and a Session opens on that file as
on the credential (the conformance kit's `placeholder` scenario).

Not guaranteed: secrecy. The adapter renders placeholders; keeping the harness from the credential is
the runtime's fence and broker. Nor that a host never echoes a credential back: the probe found none
that did on the routes named here, for the error paths a wrong credential reaches.

## Evidence

- agentd's `probes/ironproxy` (FINDINGS 9, 11, 14, 15, 16, 20): the pinned claude with a `setup-token`
  placeholder and the pinned codex with a placeholder ChatGPT login, against mocks and the live
  services, through iron-proxy behind an nftables fence.
- The conformance kit's `placeholder` scenario passes against the fake adapter, and each of its
  rules — purity, the nonce, the route, the secret kept from the file — fails against a fake adapter
  that breaks it.

## Consequences

- `pkg/contract` is `harness-adapter/1.2`. An adapter implements `Placeholder`; without the
  capability it answers `unsupported`. agentd, the one caller, moves with this release.
- A profile that renders placeholders implements `adapter.Placeholderer`; the shared adapter checks
  the request against the Descriptor and the result against the route.
- A harness upgrade that moves a host, a header or a token's shape is a change to its route or
  placeholder, verified against the broker before the pin moves.

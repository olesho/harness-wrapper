# ADR-020: Placeholder learns the agent's model

**Status:** Accepted (2026-10-06)

**Intent:** principle 3, *normalize, don't leak*, and principle 6, *evolve public contracts
deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 3, which provider a
model belongs to, and where that provider takes its key, stay in the profile; the runtime passes the
model it already holds. Under principle 6, the field is part of interface 1.5, optional, and read
only by a harness that needs it.

## Context

A credential kind's route ([ADR-014](adr-014-brokered-credentials.md)) is fixed in the Descriptor:
the hosts the harness presents the credential to, and the headers that carry it. Claude Code and
Codex take one provider's credential per kind, so one route per kind serves them.

pi takes an API key for about forty providers, one key per provider, under the provider the model
names (`--model anthropic/claude-…`): Anthropic's in `x-api-key` on `api.anthropic.com`, OpenAI's in
`Authorization` on `api.openai.com`, Gemini's in `x-goog-api-key` on
`generativelanguage.googleapis.com` (`probes/pirpc`). One kind per provider would put forty field
names before a person creating an agent. One kind, `api_key`, whose provider follows the model, puts
one, but its route has to hold every provider's hosts and headers, and a broker that swapped the key
on all of them would hand it to every provider a compromised agent wrote to.

`PlaceholderResult.Validate` already allows a swap narrower than its route: its hosts and headers
need only be a part of it. What the harness lacks is the model, to know which part.

## Decision

1. **Interface 1.5 adds `PlaceholderRequest.Model`:** the agent's model as its Agent Spec names it,
   or empty. A harness whose credential kind serves several providers narrows each swap to the hosts
   and headers of the provider the model names, and refuses a model it cannot place with
   `invalid_spec` on field `model`. A harness whose kinds serve one provider each need not read it;
   Claude Code's and Codex's profiles do not.
2. **The conformance scenario `placeholder`** asks with the fixture's model, and with a model no
   harness knows: that one is refused on field `model`, or answered within the route all the same.

## Consequences

- A runtime passes the model it already has for the agent when it asks for a placeholder; nothing
  else of its broker changes. The swap's hosts are where the broker injects the credential.
- A runtime that allows an agent the hosts of its kind's whole route, rather than those of its
  swaps, lets a Pi agent reach every provider in the route with keys of its own. agentd allows the
  swaps' hosts.

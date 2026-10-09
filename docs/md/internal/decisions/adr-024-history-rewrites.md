# ADR-024: a load may rewrite one value in a saved file's first line

**Status:** Accepted (2026-10-09). It narrows [ADR-013](adr-013-session-load-and-own-turns.md) #1:
a restore still rewrites nothing inside a saved file, but for the one value a 1.8 rewrite names.

**Intent:** principle 2, *a wrong verdict is worse than no verdict*, principle 3, *normalize, don't
leak*, and principle 6, *evolve public contracts deliberately*
([INTENT](../../../../INTENT.md#design-principles)). Under principle 2, a loaded pi Session continues
or does not open; it never becomes an empty one under the saved id. Under principle 3, which field of
which file names a working directory stays the profile's to know: the Supervisor applies a rule, and
knows no harness. Under principle 6, it is a minor version: an optional result field a 1.7 caller
never meets, since no profile renders it for a request with no `load`.

## Context

pi keeps a Session as one file, `<session dir>/<time>_<id>.jsonl`, whose first line, its header,
names the working directory pi ran in (`"cwd"`). The Pi profile puts the session dir in the config
root, so a saved Session needs no relocation. But `--session-id` finds a session only among those
whose header names pi's working directory, and an agent's workspace differs between environments.
pi 1.0.4, opened under a copied session's id in another workspace, starts a new, empty session under
that id, and says so only on stderr ([the probe](../../../../probes/pirpc/FINDINGS.md#loading-a-saved-session)).
`--session PATH` is no way round it: pi refuses a file whose header's directory does not exist, and
answers no RPC command. With the header naming the new workspace, the copy loads: the same file
goes on, tools run in the new workspace, and pi appends a system message saying so.

A Supervisor writes the restored files as the workload, from the archive, and then reads the
restored record to its end for the checkpoint the new history starts at. A profile cannot change a
file after that read without moving every offset the checkpoint names; and it could not before it,
since `Provision` is pure.

## Decision

1. **Interface 1.8 adds `ProvisionResult.history_rewrites`**, for a request that loads. A rule names
   a history path (a file or a directory, where the relocations put it), a top-level field, and the
   value `from` it replaces with `to`. In each restored file beneath the path whose first line — at
   most `MaxRewriteLine` bytes — is a JSON object whose field is a string equal to `from`, that
   value becomes `to`, encoded as JSON with no HTML escaping. Every other byte stays. A file that
   does not match is restored as it was. Rules' paths are history, no secret path, and never nested.
2. **The Supervisor applies them as it restores**, before the record is read: `contract.RewriteFirstLine`
   is the rule's normative form, and the kit's `Restore` uses it.
3. **The Pi profile rewrites the header's `cwd`** from the source's workspace, as the harness resolved
   it, to the new one, and nothing when they are one. It loads the Sessions the pinned pi saved.
4. **A loaded pi Session opens strictly.** Before pi starts, the session's file must be in the session
   dir with a header naming pi's working directory; once pi answers `get_state`, it must name that
   file and hold its messages. Either failing is `session_not_found`.

## Alternatives

- **One workspace path for every agent** (a bind mount at a fixed path). pi would find the copy as it
  is, and claude's relocation would go too; but it is the runtime's isolation, not the interface,
  that would change, and every runtime that loads would have to do it.
- **A profile hook that edits the restored files** before the record is read. It gives a profile
  write access to the environment outside `Provision`, as the workload, with no rule the Supervisor
  can check.
- **Ask pi to find a session by id in `--session-dir` whatever its header says.** Cleanest, and not
  ours to schedule; the rewrite can go once a pinned pi does it.

## Boundary

A rewrite changes one value in one line; it never moves, renames or deletes a file, and never names
a secret path. The history before the load still says where the source ran: pi's earlier system
messages name the source's workspace.

## History

- 2026-10-09: accepted, with interface 1.8 and the Pi profile's `session_load`.
- 2026-10-09: renumbered from ADR-023 and interface 1.7 when it merged after the harness version
  policy, which took both ([ADR-023](adr-023-harness-version-policy.md)). The tag v0.35.0 carries
  it as 1.7; v0.37.0 is the first release with both.

# PUPPET-236 — claude's unnumbered folder-trust dialog: cross-repo brief for meta-harness

Sibling repo: `meta-harness` (`~/Work/aether/meta-harness`, override with
`META_HARNESS_DIR`). Paired consume-side ticket: **PUPPET-296** —
*"meta-harness: parse claude's unnumbered folder-trust dialog (selector menu +
explicit detection states)"*, the TS port of this work, filed under
**PUPPET-295**. Go-side origin ticket: **PUPPET-236**.

> **Filename.** The siblings in this directory are `HARNESS-WRAPPER-<n>-<slug>.md`
> (`-77-`, `-78-`, `-101-`), numbered from harness-wrapper's retired per-repo
> tracker. This work has no HARNESS-WRAPPER number and inventing one would be a
> dangling reference, so it keeps the `<TICKET-ID>-<slug>.md` shape with the real
> fleet id. See [`APPLY.md`](APPLY.md)'s sibling list.

Like [`HARNESS-WRAPPER-77-permission-mode.md`](HARNESS-WRAPPER-77-permission-mode.md),
[`HARNESS-WRAPPER-78-sandbox-defaults-argv.md`](HARNESS-WRAPPER-78-sandbox-defaults-argv.md)
and [`HARNESS-WRAPPER-101-structured-result-permission-mode.md`](HARNESS-WRAPPER-101-structured-result-permission-mode.md),
and unlike [`APPLY.md`](APPLY.md) (HARNESS-WRAPPER-66), this is a **note, not a
byte-exact bundle**: nothing here is copied into the sibling. Everything
meta-harness must change lives in MH-owned TypeScript. It is staged in the
canonical `harness-wrapper` worktree for the same reason the others are — that is
the unit the fleet supervisor publishes, and per `APPLY.md` cross-repo work is
staged here and applied in the sibling under the paired ticket, never edited
across worktrees.

## The bug, in one line

Claude Code **2.1.251** renders the folder-trust dialog with **unnumbered**
choices, so a digit-requiring menu parser yields zero options and the caller
reads "no dialog present" — permanently, not just mid-render.

## The captured frame (Claude Code 2.1.251 folder-trust dialog)

```
Accessing workspace:
/private/tmp/trustrepo
Quick safety check: Is this a project you created or one you trust? ...
Claude Code'll be able to read, edit, and execute files here.
Security guide
 ❯ No, exit
   Yes, I trust this folder
Enter to confirm · Esc to cancel
```

Two properties of this frame are load-bearing:

- the selector glyph is **U+276F** (`❯`), not a digit;
- the highlight **defaults to the NEGATIVE choice** (`No, exit`). A bare Enter,
  or any "the first option is the affirmative one" assumption, quits claude.

## What carries over to meta-harness

Verified against `/Users/oleh/.loom/workspaces/PUPPET/meta-harness`.

- **`src/turns/harness/claudecode.ts:96` — `menuRE`.** Identical digit
  requirement to the Go side:

  ```ts
  const menuRE = /^[^\dA-Za-z\n]*(\d)\.[^\S\n]+(\S[^\n]*)$/gm;
  ```

  Nothing in the unnumbered frame matches it.

- **`src/turns/harness/claudecode.ts:456` — `DetectInput`**, with the same
  zero-options early return at **`:464`**:

  ```ts
  const opts = parseMenuOptions(text);
  if (opts.length === 0) return null; // anchor visible but menu not rendered yet
  ```

  The anchor (`trustAnchorAlt`, "Is this a project you created or one you
  trust?") matches, the parse yields nothing, and the function reports *no
  dialog*. Same bug, same fix point.

**Extra TS-only surface, in scope for meta-harness and with no Go counterpart:**
`DetectQuestion` / `questionOptionRE` (`claudecode.ts:88-92`,
`/^[^\S\r\n]*(?:❯[^\S\r\n]+)?(\d+)\.[^\S\n]+(\S[^\n]*)$/u`) handles claude's
AskUserQuestion dialogs. It carries the same digit assumption and the same
one-shape-too-few risk. Worth covering in PUPPET-296; there is nothing to mirror
from the Go side because the Go port has no equivalent.

## What does NOT carry over — `src/chat/ready.ts`

**`src/chat/ready.ts` is not vulnerable, and PUPPET-296 must not "fix" it.** An
earlier revision of the Go plan claimed the TS port shared Go's bare-`❯`
readiness hole; that claim was wrong. Two independent reasons:

1. **`claudeComposerRE` (`ready.ts:23`) requires a bare `❯` alone on its line:**

   ```ts
   const claudeComposerRE = /^[^\S\r\n]*❯[^\S\r\n]*$/m;
   ```

   The dialog's `❯ No, exit` row carries a label after the glyph, so it does not
   match. The comment at `ready.ts:19-24` says this is deliberate. Go's
   `pkg/chat/ready.go` instead ended in a bare `strings.Contains(text, "❯")`,
   which is exactly why the dialog read as READY there.

2. **`claudeBlockingDialog()` (`ready.ts:25-30`) is anchor-only** — three
   `text.includes(...)` checks against `claudeTrustAnchor` /
   `claudeTrustAnchorAlt` / `claudeBypassAnchor` (`:15-17`), consulted at
   `ready.ts:145`, with **no parse anywhere in the path**. Readiness therefore
   never depends on whether the options parse.

So the TS port blocks on the trust dialog correctly today. The scope of
PUPPET-296 is **the parser only**.

### The TS design is the stronger one — candidate follow-up for the Go side

Anchor-only readiness is the better structure: it cannot be broken by a menu
shape the parser has not learned yet, which is the entire failure mode of
PUPPET-236. The Go fix reaches the same *behaviour* by a longer route — a
four-state `DetectInputDetail` whose non-`None` states all block — but Go's
readiness still runs through the parser. Adopting the TS shape in Go (block on
the anchor, parse only to decide *what to answer*) is a **candidate follow-up on
the Go side**, not part of PUPPET-236. Recorded here so the direction of the
borrowing is not lost: for once meta-harness is the reference, not the port.

## Reference implementation

Branch **`loom/PUPPET-247`**, commit **`97aa89d`** in this repo —
*"fix(claudecode): parse the unnumbered selector menu, and stop reporting an
unparseable dialog as no dialog"*. Primary file:
**`pkg/turns/harness/claudecode/menu_selector.go`**. Read it before writing the
TS; the parts that are not obvious from the frame are:

- **Numbered-first ordering.** The numbered parser still runs first and its
  output is byte-identical to today's; the selector parser runs only when it
  yields nothing. A numbered menu also carries `❯`, and its digit keys are
  absolute (immune to a stale highlight), so it must win.
- **Scan only after the anchor offset.** `❯` is also claude's composer glyph, so
  a whole-frame scan is not deterministic.
- **Label-column blocking with an 8-row cap.** Rows qualify only at the exact
  label column of the `❯` row, non-blank, no second `❯`, stopping at blank
  lines, box borders and footer hints. This is what keeps `Security guide`, the
  path line and `Enter to confirm · Esc to cancel` from becoming options. Fewer
  than 2 or more than 8 rows → unparseable, surfaced loudly.
- **Positional keys — the dangerous half.** With `h` the highlighted row index,
  row `i` gets CR when `i == h`, `(i-h)` × `ESC [ B` + CR when below, `(h-i)` ×
  `ESC [ A` + CR when above. **Never a bare CR for a non-highlighted row** — that
  is the whole bug class. The keys must be **one write**: an `ESC [ B` split
  across writes is parsed as a lone Esc, which cancels the dialog.
- **Four explicit detection states** replace the overloaded boolean: `None` (no
  anchor), `Pending` (anchor, no choice-shaped line yet — stay silent, a
  half-painted frame is normal), `Unparseable` (anchor + choice-shaped lines that
  do not parse — fail loudly with a named cause), `OK`. The Go side added an
  `ErrUnrecognizedDialog` sentinel fired only after the state survives a re-check
  of the live screen.
- **The request id must stay highlight-independent.** Go's `inputID` hashes kind
  + prompt + option *labels* only. Folding the highlight index in would make each
  arrow keypress mint a "new" request that the policy answers again.

## Cross-links

- **PUPPET-295** — parent: the meta-harness side of PUPPET-236.
- **PUPPET-296** — the paired implementation ticket in `meta-harness`.
- **PUPPET-236** — the Go-side origin ticket (rationale and blast radius).
- **PUPPET-297** — this file.

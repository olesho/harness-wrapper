# status corpus

Captured `/status` screens — the in-repo anchor for the **labels** a harness's
status box exposes.

Today that is exactly one signal, for one harness: codex's `Permissions:` and
`Collaboration mode:` rows. Before this capture those two strings appeared
**nowhere** in the tree, which is the whole reason it exists.

## Why a capture with no parser behind it

Codex's permissions rung lives on this box, and nothing in Go reads it yet —
`(*codex.Adapter).PermissionMode` says so in as many words
(`pkg/turns/harness/codex/codex.go:184-188`: the `/status` scrape plus its
write-then-read primer are "a separate follow-up ticket"). So this corpus is
deliberately **not** wired into an offline conformance test the way
`test/corpus/permission-mode/` is: there is no shipped parser to drive it
through, and inventing one here would pre-empt that ticket.

What guards the labels *today* is the live check
`TestConformance_CodexStatusRows` (`pkg/harness/conformance_test.go`), which
launches the real binary behind `HARNESS_WRAPPER_CONFORMANCE=1`. This capture is
the frozen counterpart: it says what the box looked like when those assertions
were written, so a future reader can see what "drift" is drift *from* without
having a codex login.

When the scraper ticket lands, this is the fixture it replays.

## Layout

    <harness>/<case>/
      screen.txt   verbatim pkg/screen render of the captured /status screen
      bytes.raw    the redacted raw capture (see below)
      meta.json    { harness, binary_version, recorded_at, cols, rows,
                     permission_mode, launch_flags, labels, collaboration_mode,
                     redactions, notes }

`screen.txt`, not `expected.txt` — same reason as
`test/corpus/permission-mode/README.md` gives: `expected.txt` means "the final
assistant text" in the screenbench tree, and this is not that.

These are not conversation scenarios, so they are deliberately **not** in
`Makefile`'s `SCENARIOS` list.

## Cases

| case | cols | launch | asserted shape |
| --- | --- | --- | --- |
| `codex/read-only-untrusted` | 120 | `--permission-mode manual` → `-s read-only -a untrusted` | `Permissions: Read Only (untrusted)`, `Collaboration mode: Default` |

120 columns is load-bearing: it is the `pkg/chat` default
(`pkg/chat/conversation.go:71`), and codex degrades `/status` on narrower
terminals — a capture at a hand-picked width would pin an artefact of the
recording rig rather than what the library's own callers see.

## How the capture was made

Real capture, never synthetic. Recorded on 2026-07-23 against codex **0.144.5**
through the repo's own stack — `chat.Open` → `pkg/wrapper` PTY →
`pkg/screen` — with `Wrapper().AttachOutput` teeing the raw stream, driven
exactly the way `TestConformance_CodexStatusRows` drives it:

1. launch `--permission-mode manual` (`-s read-only -a untrusted`) at 120x40,
   with `AutoSkipCodexUpdateNotice` so the update menu cannot wedge the launch;
2. wait for the composer to be prompt-ready;
3. type `/status`, wait for the composer to echo it, then send **CSI 13 u** —
   codex ≥ 0.141.0 runs the kitty keyboard protocol, so a plain CR only inserts
   a newline (`pkg/chat/ready.go:333`, codex arm). The text and the submit key
   must be **separate writes**: sent in one burst, the Enter is swallowed by the
   slash-command popup and the command never runs;
4. stop teeing once both rows are on screen.

`bytes.raw` is truncated at the last synchronized-output frame end
(`ESC[?2026l`), the same rule `test/corpus/permission-mode/` used for its codex
capture — an untruncated stream ends in teardown and renders to a blank screen.

No turn was ever sent, so the capture cost no API tokens. `/permissions` was
**never** opened: selecting a preset there writes `~/.codex/config.toml`
globally.

The working directory was a scratch `/private/tmp` path, so the box carries no
repo path, no home directory and no `AGENTS.md` from this checkout.

## Redaction

Two values in `bytes.raw` were rewritten with **equal-length** placeholders
before committing — the account address (→ `user@example.com`) and the session
uuid (→ an all-zero uuid). Equal length is what keeps the capture honest: the
box's column alignment is byte-identical to what codex painted, and rendering
the committed `bytes.raw` through `pkg/screen` at 120x40 still reproduces
`screen.txt` exactly. `meta.json`'s `redactions` field lists them. Nothing else
was edited.

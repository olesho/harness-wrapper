# pi's RPC mode: the probe

What can a client learn from `pi --mode rpc` about each input it sends, and what does pi's session
file hold, after a crash too? This probe answers that for the pi a Harness Adapter profile would
pin, by speaking pi's RPC itself, with no adapter between, against the mock model API. What it
found is in [FINDINGS.md](FINDINGS.md); the Harness Adapter's Pi profile is built on those
findings, and each is a test here, so a pi that behaves otherwise fails this probe first.

It is Step 0 of
[agentd: Pi as a third harness](https://coplan.olehluchkiv.com/d/agentd-pi-as-a-third-harness),
whose gate it decides.

## Run it

The probe is a Go test package that skips without the binary it names. `fetch.sh` downloads a pi
release for this platform and checks it against the release's `SHA256SUMS`:

```sh
probes/pirpc/fetch.sh /tmp/pinned 1.0.4
HW_REAL_PI=/tmp/pinned/pi/pi go test -count=1 -v ./probes/pirpc/
```

Without a version, `fetch.sh` takes pi's pin in `pkg/versions/versions.json`. On another machine,
build the test binary (`GOOS=linux GOARCH=arm64 go test -c ./probes/pirpc/`) and run it from a
directory holding `hwtag.ts`. `pgrep` and `pkill` must be on `PATH`.

`-v` prints every command, answer and event with its time, and the session files, which are the
evidence behind each finding. No account and no network: the model is `internal/mockapi`, on the
Anthropic Messages API and the OpenAI Responses API, and `TestBehindABroker` runs its own broker,
an HTTPS proxy with a CA of its own that swaps placeholders for keys.

| Test | What it establishes |
|---|---|
| `TestARun` | A run on each API: its receipt, its end, and the session's tag, user message and answer, in order. |
| `TestReceipt` | A prompt while busy, and one without a key, are refused and never run; the busy one leaves its tag. |
| `TestReopen` | `--session-id` reopens its session after stdin's end; with the file gone, it starts a new one. |
| `TestTags` | The tag extension shows in `get_commands`; what a tagged input starting with `/` becomes; a tag without the extension. |
| `TestOutcomes` | How an exhausted error, and an abort mid-stream, before the first token and mid-tool, end in the session. |
| `TestRetry` | A retried error: its events, and its trace in the session. |
| `TestErrorTexts` | The text each error ends with, usage walls included, on both APIs. |
| `TestCrashes` | What the session holds after a kill mid-stream, mid-tool and right after a prompt; a reopen after one. |
| `TestStop` | stdin's end and SIGTERM mid-tool: pi's exit, and the tool's command. |
| `TestMCPAtOpen` | pi connects to MCP servers when it starts; `!command` headers; direct exposure. |
| `TestModelOutsideTheCatalog` | A model pi's catalog lacks, with and without a `models.json` entry. |
| `TestDistribution` | Which of a release's files pi needs. |
| `TestBehindABroker` | pi through an HTTPS proxy with its own CA: which CA variables work, each key's header, the hosts pi asks for. |

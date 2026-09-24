# claude-code over stream-json

Captures of claude 2.1.281 driven through its persistent stream-json transport
(`claude -p --input-format stream-json --output-format stream-json --verbose`),
against a local stand-in for the Messages API: no account, no tokens. Recorded
for agentd's P11 driver probe (github.com/olesho/agentd, `probes/p11`), which
holds the recorder and the full set.

Each file is JSON lines: a header (`claude_version`, `scenario`), then every
frame in time order as `{"t": seconds, "dir": "in"|"out", "frame": {...}}`,
plus `note` lines. Machine paths are replaced by `<CWD>`, `<CFG>` and
`<PID>`.

`pkg/chat/stream_corpus_test.go` replays the `out` frames through the
stream-json driver's frame handling (ADR-009) and checks what each turn ends
as. A new claude version is checked by re-recording these and re-running it.

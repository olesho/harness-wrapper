# Origin

The transcript line-parsing helpers in this package are a port of
`cmd/entire/cli/transcript/` and the `StripIDEContextTags` helper from
`cmd/entire/cli/textutil/` in the [entireio/cli](https://github.com/entireio/cli)
project, by way of loomcli's `internal/sessions/transcript`.

## Upstream

- Repository: https://github.com/entireio/cli
- License: MIT (Copyright (c) 2026 Entire Inc.) — reproduced in `LICENSE.upstream`.

## Scope of the port

- `line.go` — Line, AssistantMessage, ContentBlock, ToolInput, UserMessage, and
  the Type/ContentType constants.
- `parse.go` — ParseFromBytes, ParseFromFileAtLine, SliceFromLine,
  ExtractUserContent, normalizeLineType.
- `strip_tags.go` — inlined StripIDEContextTags.
- `claudecode/parse_claude.go` — Claude Code's line→Event parser
  (Events/userLineEvents/assistantLineEvents), which produces this package's
  canonical `Event` (one event per content block, tool-aware).

The canonical `Event` (event.go) is field-compatible with loomcli's promoted
Event, so this parser yields output equivalent to loomcli's `claude.Events`.

## MIT License text

Reproduced in `LICENSE.upstream` per the MIT license's requirement that the
notice be included in copies or substantial portions of the Software.

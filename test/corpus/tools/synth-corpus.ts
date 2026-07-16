// Port of internal/screenbench/cmd/screenbench-synth/main.go: generates a
// synthetic bake-off corpus exercising common TUI byte-stream patterns that
// real chat CLIs emit: SGR styling, cursor moves, partial-line rewrites,
// alternate-screen toggles, code blocks, scrollback overflow, box-drawing
// panels.
//
// Output goes under <out>/synth/<scenario>/ with bytes.raw, meta.json, and
// expected.txt populated. The expected text is what a faithful emulator
// should render after the full byte stream replays, after metrics.normalize
// (trailing whitespace trimmed per line; blank lines trimmed top/bottom).
//
// These recordings do NOT represent real Codex/Claude Code output -- they
// validate the bench plumbing and surface emulator behavior gaps on
// isolated TUI primitives. Real-harness recordings are still required to
// answer the actual fidelity question.

import { mkdir, writeFile } from "node:fs/promises"
import * as path from "node:path"

import { type Meta, writeMeta } from "./screenbench/scenario.ts"

const COLS = 80
const ROWS = 24

interface Synth {
  name: string
  notes: string
  bytes: Buffer
  expected: string
}

// The committed corpus's expected.txt files have trailing per-line
// whitespace stripped (an artifact of how they were committed) even where
// the in-memory `expected` strings above don't -- e.g. codeBlock's blank
// gutter lines. Match that at the file-write boundary only.
function trimTrailingWhitespacePerLine(s: string): string {
  return s.replace(/[ \t]+$/gm, "")
}

function parseArgs(argv: string[]): { out: string } {
  let out = "test/corpus"
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]
    if (arg === "--out") {
      out = argv[++i]
    } else if (arg.startsWith("--out=")) {
      out = arg.slice("--out=".length)
    }
  }
  return { out }
}

/** A minimal two-turn exchange with simple SGR styling. */
function shortReply(): Synth {
  let b = ""
  b += "\x1b[2J\x1b[H" // clear screen, cursor home
  b += "\x1b[1m> \x1b[22mhello\r\n" // bold prompt, user text
  b += "\x1b[32mHi there!\x1b[0m " // green assistant
  b += "How can I help today?\r\n"
  b += "\x1b[1m> \x1b[22m\x1b[?25h" // new prompt, show cursor
  return {
    name: "short-reply",
    notes: "single-turn short reply with SGR styling",
    bytes: Buffer.from(b, "utf8"),
    expected: "> hello\nHi there! How can I help today?\n>",
  }
}

/** A multi-paragraph reply with headings, bold, and bullets. */
function longMarkdown(): Synth {
  let b = ""
  b += "\x1b[2J\x1b[H"
  b += "\x1b[1m> \x1b[22msummarize the plan\r\n"
  b += "\x1b[1;4mOverview\x1b[0m\r\n" // bold+underline heading
  b += "The plan has \x1b[1mthree phases\x1b[22m. Each phase is\r\n"
  b += "independently shippable.\r\n"
  b += "\r\n"
  b += "\x1b[1;4mPhases\x1b[0m\r\n"
  b += "  \x1b[33m•\x1b[0m \x1b[1mDiscovery\x1b[22m — corpus + bench\r\n"
  b += "  \x1b[33m•\x1b[0m \x1b[1mAdapters\x1b[22m — codex, claude-code\r\n"
  b += "  \x1b[33m•\x1b[0m \x1b[1mLibrary\x1b[22m — pkg/chat\r\n"
  b += "\r\n"
  b += "\x1b[2mEnd of summary.\x1b[0m\r\n"
  b += "\x1b[1m> \x1b[22m"
  const exp = [
    "> summarize the plan",
    "Overview",
    "The plan has three phases. Each phase is",
    "independently shippable.",
    "",
    "Phases",
    "  • Discovery — corpus + bench",
    "  • Adapters — codex, claude-code",
    "  • Library — pkg/chat",
    "",
    "End of summary.",
    ">",
  ].join("\n")
  return {
    name: "long-markdown",
    notes: "multi-paragraph reply: headings, bold runs, bullet list",
    bytes: Buffer.from(b, "utf8"),
    expected: exp,
  }
}

/** Right-justifies n in a 3-character-wide field, mirroring Go's "%3d". */
function pad3(n: number): string {
  return String(n).padStart(3, " ")
}

/**
 * A fenced code block rendered with line gutters and SGR syntax
 * highlighting. The expected output strips the colors but preserves layout.
 */
function codeBlock(): Synth {
  let b = ""
  b += "\x1b[2J\x1b[H"
  b += "\x1b[1m> \x1b[22mshow a hello world\r\n"
  b += "\x1b[36m```go\x1b[0m\r\n"
  const lines = [
    "package main",
    "",
    'import "fmt"',
    "",
    "func main() {",
    '    fmt.Println("hello, world")',
    "}",
  ]
  for (let i = 0; i < lines.length; i++) {
    const ln = lines[i]
    // gutter: dim gray "  1 │ "
    b += `\x1b[2m${pad3(i + 1)} │\x1b[0m `
    // naive "highlight" certain keywords just to exercise SGR
    let col = ln.split("package").join("\x1b[35mpackage\x1b[0m")
    col = col.split("import").join("\x1b[35mimport\x1b[0m")
    col = col.split("func").join("\x1b[35mfunc\x1b[0m")
    b += col
    b += "\r\n"
  }
  b += "\x1b[36m```\x1b[0m\r\n"
  b += "\x1b[1m> \x1b[22m"

  let exp = "> show a hello world\n```go\n"
  for (let i = 0; i < lines.length; i++) {
    exp += `${pad3(i + 1)} │ ${lines[i]}\n`
  }
  exp += "```\n>"

  return {
    name: "code-block",
    notes: "fenced code block with line-number gutter and SGR keyword highlighting",
    bytes: Buffer.from(b, "utf8"),
    expected: exp,
  }
}

/**
 * Assistant starts streaming, then the stream is erased mid-line (\r + EL)
 * and replaced with an "interrupted" message. This pattern is common in
 * chat TUIs when the user hits Ctrl-C.
 */
function interruptMidStream(): Synth {
  let b = ""
  b += "\x1b[2J\x1b[H"
  b += "\x1b[1m> \x1b[22mwrite a long story\r\n"
  b += "Once upon a time, in a land far away, there lived a curious"
  // User interrupts: \r + EL (erase to end of line) replaces the line.
  b += "\r\x1b[2K"
  b += "\x1b[31m⚠ interrupted by user\x1b[0m\r\n"
  b += "\x1b[1m> \x1b[22m"
  const exp = ["> write a long story", "⚠ interrupted by user", ">"].join("\n")
  return {
    name: "interrupt-mid-stream",
    notes: "partial assistant line erased via \\r+CSI2K and replaced with interrupt notice",
    bytes: Buffer.from(b, "utf8"),
    expected: exp,
  }
}

/**
 * Enter alt screen, display a "loading" frame, leave alt screen, then show
 * real content on the main screen. The alt-screen content should NOT appear
 * in the final main-screen extraction.
 */
function altScreenToggle(): Synth {
  let b = ""
  b += "\x1b[2J\x1b[H"
  b += "\x1b[1m> \x1b[22mlist files\r\n"
  // Enter alt screen, draw spinner frame.
  b += "\x1b[?1049h\x1b[2J\x1b[H"
  b += "⠋ loading files...\r\n"
  // Leave alt screen -- main-screen content should be restored.
  b += "\x1b[?1049l"
  b += "README.md  go.mod  pkg/\r\n"
  b += "\x1b[1m> \x1b[22m"
  const exp = ["> list files", "README.md  go.mod  pkg/", ">"].join("\n")
  return {
    name: "alt-screen-toggle",
    notes: "CSI ?1049h/l alt-screen entry/exit; alt-screen content must not bleed into main extraction",
    bytes: Buffer.from(b, "utf8"),
    expected: exp,
  }
}

/**
 * Emits more lines than the terminal height. A faithful emulator's visible
 * screen should retain only the last `rows` lines after the stream
 * completes. We exercise this with a 30-line numbered list on a 24-row
 * terminal.
 */
function scrollbackOverflow(): Synth {
  let b = ""
  b += "\x1b[2J\x1b[H"
  b += "\x1b[1m> \x1b[22mcount to 30\r\n"
  for (let i = 1; i <= 30; i++) {
    b += `line ${i}\r\n`
  }
  b += "\x1b[1m> \x1b[22m"

  // rows = 24. The screen holds 24 lines. After all writes the screen's
  // visible window is the *last 24 lines*. Compute what they are.
  // We emitted: "> count to 30", "line 1" .. "line 30", "> "
  // That's 32 logical lines; the last line is the prompt with no newline.
  // Visible 24 = last 24 rows. The cursor sits on the prompt row.
  // Expected (after normalize trims trailing whitespace and blank-line edges):
  const all = ["> count to 30"]
  for (let i = 1; i <= 30; i++) {
    all.push(`line ${i}`)
  }
  all.push(">")
  const visible = all.slice(all.length - ROWS)
  return {
    name: "scrollback-overflow",
    notes: "32 logical lines on a 24-row screen; visible region is the trailing 24",
    bytes: Buffer.from(b, "utf8"),
    expected: visible.join("\n"),
  }
}

async function main(): Promise<void> {
  const { out } = parseArgs(process.argv.slice(2))

  const scenarios: Synth[] = [
    shortReply(),
    longMarkdown(),
    codeBlock(),
    interruptMidStream(),
    altScreenToggle(),
    scrollbackOverflow(),
  ]

  for (const s of scenarios) {
    const dir = path.join(out, "synth", s.name)
    await mkdir(dir, { recursive: true })
    await writeFile(path.join(dir, "bytes.raw"), s.bytes)
    // The committed corpus's expected.txt files each end with a trailing
    // newline (an artifact of how they were committed) even though the
    // in-memory `expected` strings above don't end in one; add it only at
    // the file-write boundary so a regenerated corpus is byte-identical.
    await writeFile(path.join(dir, "expected.txt"), `${trimTrailingWhitespacePerLine(s.expected)}\n`)
    const meta: Meta = {
      harness: "synth",
      binaryVersion: "screenbench-synth",
      recordedAt: new Date().toISOString(),
      cols: COLS,
      rows: ROWS,
      notes: s.notes,
    }
    await writeMeta(dir, meta)
    console.log(`wrote ${dir} (${s.bytes.length} bytes)`)
  }
}

main().catch((err) => {
  console.error(`screenbench-synth: ${err instanceof Error ? err.message : String(err)}`)
  process.exitCode = 1
})

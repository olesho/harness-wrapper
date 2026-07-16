// Port of internal/screenbench/metrics/metrics.go's test coverage for the TS
// port at ../screenbench/metrics.ts.

import { describe, expect, test } from "vitest"

import { exactMatch, levenshtein, normalize, normalizedDistance, stripAnsi } from "./metrics.ts"

describe("stripAnsi", () => {
  test("removes CSI escape sequences", () => {
    expect(stripAnsi("\x1b[1mbold\x1b[0m plain")).toBe("bold plain")
    expect(stripAnsi("\x1b[2J\x1b[Hhello")).toBe("hello")
  })

  test("leaves plain text untouched", () => {
    expect(stripAnsi("hello world")).toBe("hello world")
  })
})

describe("normalize", () => {
  test("strips ANSI, expands tabs, trims trailing whitespace per line", () => {
    const input = "\x1b[1ma\tb\x1b[0m\r\nworld   \r\n"
    expect(normalize(input)).toBe("a    b\nworld")
  })

  test("trims blank lines from start and end but not the middle", () => {
    const input = "\n\nfirst\n\nmiddle\n\nlast\n\n\n"
    expect(normalize(input)).toBe("first\n\nmiddle\n\nlast")
  })

  test("empty input normalizes to empty string", () => {
    expect(normalize("")).toBe("")
    expect(normalize("\n\n\n")).toBe("")
  })
})

describe("exactMatch", () => {
  test("matches after normalization differences", () => {
    expect(exactMatch("hello   \r\nworld\n\n", "hello\nworld")).toBe(true)
  })

  test("does not match differing content", () => {
    expect(exactMatch("hello", "world")).toBe(false)
  })
})

describe("levenshtein", () => {
  test("distance from empty string is the other string's length", () => {
    expect(levenshtein("", "")).toBe(0)
    expect(levenshtein("", "abc")).toBe(3)
    expect(levenshtein("abc", "")).toBe(3)
  })

  test("classic edit distance examples", () => {
    expect(levenshtein("kitten", "sitting")).toBe(3)
    expect(levenshtein("flaw", "lawn")).toBe(2)
    expect(levenshtein("abc", "abc")).toBe(0)
  })

  test("counts in codepoints, not UTF-16 code units (astral-plane input)", () => {
    // U+1F600 GRINNING FACE is a surrogate pair in UTF-16 (2 code units)
    // but a single codepoint. A UTF-16-code-unit-based implementation
    // would report a distance of 2 from the empty string instead of 1.
    expect(levenshtein("", "\u{1F600}")).toBe(1)
    expect(levenshtein("", "\u{1F600}\u{1F601}")).toBe(2)

    // Same astral-plane characters, comparing a run against itself minus
    // its last character -- exercises codepoint-based (not code-unit-based)
    // length counting in the DP table dimensions.
    const emojiRun = "\u{1F600}\u{1F601}\u{1F602}"
    expect(levenshtein(emojiRun, "\u{1F600}\u{1F601}")).toBe(1)
  })
})

describe("normalizedDistance", () => {
  test("identical strings have distance 0", () => {
    expect(normalizedDistance("hello", "hello")).toBe(0)
  })

  test("both-empty (after normalize) returns 0", () => {
    expect(normalizedDistance("", "")).toBe(0)
    expect(normalizedDistance("\n\n", "\n\n")).toBe(0)
  })

  test("divides edit distance by the longer normalized codepoint count", () => {
    // normalize() is a no-op for these plain strings.
    // "abc" vs "abd": distance 1, longer length 3.
    expect(normalizedDistance("abc", "abd")).toBeCloseTo(1 / 3)
  })

  test("uses codepoint counts for astral-plane input", () => {
    const a = "\u{1F600}\u{1F601}\u{1F602}"
    const b = "\u{1F600}\u{1F601}"
    // distance 1, longer normalized codepoint length 3
    expect(normalizedDistance(a, b)).toBeCloseTo(1 / 3)
  })
})

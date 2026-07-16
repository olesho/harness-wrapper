// Port of internal/screenbench/metrics/metrics.go: comparison utilities for
// the screen emulator bake-off. The bench compares emulator-extracted screen
// text against hand-curated ground truth using a small set of stable metrics.

// Matches ANSI CSI escape sequences (cursor moves, SGR, etc.). Used only as a
// defensive fallback -- emulator output should already be plain text.
const ansiCSI = /\x1b\[[0-9;?]*[a-zA-Z]/g

/** Removes ANSI escape sequences from s. */
export function stripAnsi(s: string): string {
  return s.replace(ansiCSI, "")
}

/**
 * Collapses trailing whitespace per line, trims leading/trailing blank
 * lines, and converts tabs to spaces. Used before comparison so emulator
 * differences in padding don't dominate edit distance.
 */
export function normalize(s: string): string {
  s = stripAnsi(s)
  s = s.split("\t").join("    ")
  const lines = s.split("\n")
  for (let i = 0; i < lines.length; i++) {
    lines[i] = lines[i].replace(/[ \r]+$/, "")
  }
  // trim blank lines top/bottom
  let start = 0
  let end = lines.length
  while (start < end && lines[start] === "") {
    start++
  }
  while (end > start && lines[end - 1] === "") {
    end--
  }
  return lines.slice(start, end).join("\n")
}

/** Reports whether two normalized strings are byte-identical. */
export function exactMatch(a: string, b: string): boolean {
  return normalize(a) === normalize(b)
}

/**
 * Returns the edit distance between two strings, counted in codepoints.
 * Implementation is O(len(a)*len(b)) time and O(min) space.
 */
export function levenshtein(a: string, b: string): number {
  let ar = Array.from(a)
  let br = Array.from(b)
  if (ar.length === 0) {
    return br.length
  }
  if (br.length === 0) {
    return ar.length
  }
  // Make ar the shorter to reduce memory.
  if (ar.length > br.length) {
    ;[ar, br] = [br, ar]
  }
  let prev = new Array<number>(ar.length + 1)
  let curr = new Array<number>(ar.length + 1)
  for (let i = 0; i <= ar.length; i++) {
    prev[i] = i
  }
  for (let j = 1; j <= br.length; j++) {
    curr[0] = j
    for (let i = 1; i <= ar.length; i++) {
      const cost = ar[i - 1] === br[j - 1] ? 0 : 1
      const ins = curr[i - 1] + 1
      const del = prev[i] + 1
      const sub = prev[i - 1] + cost
      let m = ins
      if (del < m) m = del
      if (sub < m) m = sub
      curr[i] = m
    }
    ;[prev, curr] = [curr, prev]
  }
  return prev[ar.length]
}

/**
 * Returns Levenshtein(a,b) / max(codepoints(a), codepoints(b)), clamped to
 * [0,1]. 0 means identical, 1 means fully different.
 */
export function normalizedDistance(a: string, b: string): number {
  a = normalize(a)
  b = normalize(b)
  const d = levenshtein(a, b)
  let max = Array.from(a).length
  const bLen = Array.from(b).length
  if (bLen > max) max = bLen
  if (max === 0) return 0
  return d / max
}

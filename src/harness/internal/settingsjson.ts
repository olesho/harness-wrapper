// Generic settings.json hooks-merge format shared by Claude and Gemini.
//
// Ported from harness-wrapper's pkg/harness/settingsjson.go. Claude and Gemini
// share the SAME settings.json hook format — a two-level grouping of matcher →
// command entries:
//
//   {"hooks": {"<Event>": [{"matcher": "<m>", "hooks": [{"type":"command","command":"..."}]}]}}
//
// so the merge lives here, generic; each harness's own ensure-config call is a
// thin wrapper supplying its config path + native event names.
//
// This file has NO imports from anywhere else in src/harness/** besides its
// sibling hookensure.ts (created in the same change, and itself stdlib-only) —
// matching the Go original's package-local (no cross-file dependency on the
// REST of pkg/harness) shape. HookSpec/HookEntry are defined locally below
// rather than imported from a hooks.ts port (which doesn't exist yet as part
// of this change) — they mirror the shape of Go's hooks.go HookSpec/HookEntry.

import { isManagedHookCommand, renderHookCommand, withLockedFile } from "./hookensure.ts"

/** One native hook event → meta-harness subcommand mapping. Mirrors Go's HookEntry (hooks.go). */
export interface HookEntry {
  /** The harness-native hook name written to the config (e.g. "SessionStart", "PreToolUse"). */
  nativeEvent: string
  /** Tool matcher (e.g. "Task"); empty means match all. */
  matcher: string
  /** The `<entrypoint> hooks <harness> <arg>` subcommand the fired hook invokes. */
  arg: string
}

/** The hook entries an ensure call idempotently installs. Mirrors Go's HookSpec (hooks.go). */
export interface HookSpec {
  /** Worktree-relative path to the hook config (e.g. ".claude/settings.json"). */
  configPath: string
  /** Lifecycle hooks this port manages. */
  events: HookEntry[]
  /** Optional pre-tool guard enabling cooperative preemption. */
  yield?: HookEntry | null
  /** Owner/version marker stamped on managed entries. */
  owner: string
}

/** A single command hook within a matcher group. */
interface SettingsHookCmd {
  type: string
  command: string
}

/** One matcher group in a settings.json hook event. */
interface SettingsHookMatcher {
  matcher: string
  hooks: SettingsHookCmd[]
}

/**
 * Idempotently + atomically installs spec's hooks into a Claude/Gemini-style
 * settings.json at settingsPath, rendering each command from argv via
 * renderHookCommand (harnessName is the token in the command). Preserves the
 * user's hooks + unknown keys, marks this port's entries (spec.owner), and
 * refreshes the entry-point path each call (self-healing). Lock-guarded +
 * atomic (see withLockedFile).
 */
export function ensureSettingsJSONHooks(settingsPath: string, spec: HookSpec, argv: readonly string[], harnessName: string): void {
  withLockedFile(settingsPath, (existing) => {
    const { settings, hooks } = loadSettingsJSON(existing)
    // Group managed entries by native event FIRST: one native event can carry
    // several matchers (e.g. Claude's PreToolUse has both a Task-matched
    // capture hook AND the all-matcher yield-guard), and the upsert removes
    // all managed matchers for an event before re-adding — so they must be
    // added together or the second add would delete the first.
    const byEvent = new Map<string, SettingsHookMatcher[]>()
    const add = (e: HookEntry): void => {
      const cmd = renderHookCommand(argv, harnessName, e.arg, spec.owner)
      const list = byEvent.get(e.nativeEvent) ?? []
      list.push({ matcher: e.matcher, hooks: [{ type: "command", command: cmd }] })
      byEvent.set(e.nativeEvent, list)
    }
    for (const e of spec.events) add(e)
    if (spec.yield) add(spec.yield)
    for (const [nativeEvent, matchers] of byEvent) {
      upsertSettingsHooks(hooks, nativeEvent, matchers)
    }
    return marshalSettingsJSON(settings, hooks)
  })
}

/** Parses the settings file into (top-level object, hooks object), tolerating an absent/empty file. */
function loadSettingsJSON(data: Buffer | null): { settings: Record<string, unknown>; hooks: Record<string, unknown> } {
  let settings: Record<string, unknown> = {}
  if (data !== null && data.length > 0) {
    let parsed: unknown
    try {
      parsed = JSON.parse(data.toString("utf8"))
    } catch (err) {
      throw new Error(`hooks: parse settings.json: ${(err as Error).message}`)
    }
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      throw new Error("hooks: parse settings.json: not an object")
    }
    settings = parsed as Record<string, unknown>
  }
  let hooks: Record<string, unknown> = {}
  if (settings.hooks !== undefined) {
    const raw = settings.hooks
    if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
      throw new Error("hooks: parse hooks block: not an object")
    }
    hooks = raw as Record<string, unknown>
  }
  return { settings, hooks }
}

/** Replaces all managed matchers for nativeEvent with the fresh managedMatchers, preserving every user matcher. */
function upsertSettingsHooks(hooks: Record<string, unknown>, nativeEvent: string, managedMatchers: SettingsHookMatcher[]): void {
  let matchers: SettingsHookMatcher[] = []
  if (hooks[nativeEvent] !== undefined) {
    const raw = hooks[nativeEvent]
    if (!Array.isArray(raw)) {
      throw new Error(`hooks: parse ${nativeEvent} entries: not an array`)
    }
    matchers = raw as SettingsHookMatcher[]
  }
  const kept = matchers.filter((m) => !matcherIsManaged(m))
  kept.push(...managedMatchers)
  hooks[nativeEvent] = kept
}

/** Reports whether a matcher group contains a managed command (so it is replaced on re-ensure). */
function matcherIsManaged(m: SettingsHookMatcher): boolean {
  return (m.hooks ?? []).some((h) => isManagedHookCommand(h.command))
}

/** Writes hooks back under settings["hooks"] and renders the whole object (stable, indented, trailing newline). */
function marshalSettingsJSON(settings: Record<string, unknown>, hooks: Record<string, unknown>): Buffer {
  settings.hooks = hooks
  return Buffer.from(JSON.stringify(settings, null, 2) + "\n", "utf8")
}

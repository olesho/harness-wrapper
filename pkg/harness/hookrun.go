package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// spoolSeq disambiguates spool filenames for concurrent writers within ONE
// process (the wall clock can repeat a nanosecond under contention); pid
// disambiguates across the separate hook subprocesses.
var spoolSeq atomic.Uint64

// HW_* env var names. The orchestrator SETS them in the harness launch env, the
// harness propagates them to hook subprocesses, and HandleHookEvent READS them.
// They are the authority for the hook ENVIRONMENT (never the subprocess cwd).
const (
	EnvSpool     = "HW_EVENT_SPOOL"        // spool dir; absent ⇒ handler inert
	EnvHookCwd   = "HW_HOOK_CWD"           // harness working dir (worktree)
	EnvHome      = "HW_HOME"               // user home
	EnvConfigDir = "HW_HARNESS_CONFIG_DIR" //nolint:gosec // env var NAME, not a credential
	// EnvHarnessSessionID is the native session id a RESUME launch is resuming.
	// Set only on resume (and only when non-empty); it arms the resume session
	// guard in HandleHookEvent that drops a stale/leftover hook fired for a
	// DIFFERENT session lingering in the same per-run spool. Absent (fresh
	// starts, codex, any non-resume launch) ⇒ the guard is disarmed.
	EnvHarnessSessionID = "HW_HARNESS_SESSION_ID" //nolint:gosec // env var NAME, not a credential
)

// HandleHookEvent is the entrypoint the thin `loom hooks <harness> <event>`
// command delegates to. For capture events it parses the fired hook's stdin
// payload into events and writes them to the spool; for the yield-guard control
// event it returns a HookOutcome telling the caller whether to BLOCK the tool.
//
// It is INERT (zero outcome, writes nothing) when HW_EVENT_SPOOL is absent — so
// a leftover hook entry can never perturb a non-wrapper run (review #5; this is
// the runtime counterpart to the rendered shell guard). The subprocess does NOT
// call Resolve: it obtains the harness's STATIC HookProvider and trusts that the
// main run's resolution already decided to install hooks.
func HandleHookEvent(harnessName, event string, env []string, stdin []byte) (HookOutcome, error) {
	spool := EnvLookup(env, EnvSpool)
	if spool == "" {
		return HookOutcome{}, nil // inert outside a wrapper run
	}
	p, ok := For(harnessName)
	if !ok {
		return HookOutcome{}, fmt.Errorf("harness: no profile registered for %q", harnessName)
	}
	shp, ok := p.(StaticHookProfile)
	if !ok {
		return HookOutcome{}, fmt.Errorf("harness: %q has no hook provider", harnessName)
	}
	hp := shp.StaticHookProvider()

	// The yield-guard is a control hook, not a capture hook: it decides whether
	// to block the tool, and never touches the spool.
	if spec := hp.HookSpec(); spec.Yield != nil && event == spec.Yield.Arg {
		return checkYield(EnvLookup(env, EnvYieldFile)), nil
	}

	ctx := HookContext{
		Cwd:              EnvLookup(env, EnvHookCwd),
		Home:             EnvLookup(env, EnvHome),
		ConfigDir:        EnvLookup(env, EnvConfigDir),
		SpoolDir:         spool,
		HarnessSessionID: EnvLookup(env, EnvHarnessSessionID),
	}
	events, err := hp.ParseHookPayload(ctx, event, stdin)
	if err != nil {
		return HookOutcome{}, fmt.Errorf("harness: parse hook %s/%s: %w", harnessName, event, err)
	}
	events = filterResumeSession(ctx.HarnessSessionID, events)
	if len(events) == 0 {
		return HookOutcome{}, nil
	}
	return HookOutcome{}, writeSpool(spool, event, events)
}

// writeSpool writes one batch of parsed events to the spool as a single file,
// crash-safely: marshal → write a unique `.tmp` → rename(2) into place. The
// rename is atomic and lock-free, so concurrent hook subprocesses (e.g.
// overlapping PostToolUse[Task] + Stop) never produce a partial or torn file,
// and the orchestrator's drain (which reads only completed `.json` files) never
// sees half a record (review #6).
func writeSpool(spoolDir, event string, events []transcript.ParsedEvent) error {
	// DURABLE form: persists Source/NativeID/SchemaVersion (Event's public JSON
	// omits them), which the authority filter + dedup need after the round-trip.
	data, err := transcript.MarshalParsedEvents(events)
	if err != nil {
		return fmt.Errorf("harness: marshal spool events: %w", err)
	}
	// Unique per (event, time, pid, in-process seq): pids differ across the
	// separate hook subprocesses, and the atomic seq guarantees uniqueness for
	// concurrent writers within one process even at the same nanosecond.
	// Ordering is reconstructed from event content, not the filename.
	base := fmt.Sprintf("%s-%d-%d-%d.json", event, time.Now().UnixNano(), os.Getpid(), spoolSeq.Add(1))
	final := filepath.Join(spoolDir, base)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("harness: write spool temp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("harness: commit spool file: %w", err)
	}
	return nil
}

// DrainSpool reads every COMPLETED spool file (`.json`, never the in-flight
// `.tmp`), returning all parsed events and removing each file it successfully
// consumed so a later drain does not re-emit them. The orchestrator calls it
// (after the harness exits — the grace-window drain) and may call it
// periodically; the consumer additionally dedups by Event.ID(), so a file left
// behind by a delete failure is absorbed rather than duplicated.
//
// A missing spool dir is not an error (no hooks fired). A single unreadable /
// unparseable file is skipped (left in place) and collected into err, but does
// not abort the drain of the rest.
func DrainSpool(spoolDir string) ([]transcript.ParsedEvent, error) {
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("harness: read spool dir: %w", err)
	}
	var (
		out  []transcript.ParsedEvent
		errs []string
	)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue // skip dirs and in-flight *.json.tmp files
		}
		path := filepath.Join(spoolDir, name)
		data, rerr := os.ReadFile(path) //nolint:gosec // path is inside the wrapper-owned spool dir
		if rerr != nil {
			errs = append(errs, rerr.Error())
			continue
		}
		evs, uerr := transcript.UnmarshalParsedEvents(data)
		if uerr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, uerr))
			continue // leave malformed file in place for inspection
		}
		out = append(out, evs...)
		_ = os.Remove(path) // consumed; dedup-by-ID covers a failed remove
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("harness: spool drain skipped %d file(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return out, nil
}

// filterResumeSession is the resume session guard (item 4 of the
// HARNESS-WRAPPER-52 plan). On a RESUME launch — expected != "" (from
// HW_HARNESS_SESSION_ID, set only on resume) — it drops a PARENT-conversation
// event whose session id mismatches: a stale/leftover hook fired for a
// DIFFERENT session that lingers in the SAME per-run spool. It bites only on
// resume within one worktree; the per-run temp spool (os.MkdirTemp "hw-spool-"
// + deferred RemoveAll) already isolates unrelated sessions, so this defends
// only the narrow residual where a resume reuses the spool across a session-id
// change. It mirrors TS's sessionMatches(expected, payload.session_id), which
// likewise bites only for a non-empty expected id.
//
// HandleHookEvent is the shared entrypoint for EVERY harness's hooks, so this
// also runs on the codex hook path — harmlessly: HW_HARNESS_SESSION_ID is set
// only on Claude resume launches, so for codex (and every fresh start) expected
// is empty and the guard is disarmed, returning events unchanged.
//
// SUBAGENT-SAFETY: subagent events carry their OWN native agentID in
// HarnessSessionID and the parent id in ParentSessionID (readSubagentTranscript),
// whereas parent events carry the fired session id in HarnessSessionID and an
// empty ParentSessionID (readParentTranscript / sessionMarker). A naive
// HarnessSessionID == expected filter would therefore DROP EVERY SUBAGENT EVENT
// on a resume (each subagent's id differs from the expected parent id), silently
// discarding legitimate nested runs. The only correct drop condition is a
// PARENT event (ParentSessionID == "") whose id mismatches; subagent events
// (ParentSessionID != "") are ALWAYS kept.
func filterResumeSession(expected string, events []transcript.ParsedEvent) []transcript.ParsedEvent {
	if expected == "" {
		return events // disarmed: fresh start / non-resume / codex
	}
	kept := events[:0]
	for _, pe := range events {
		if pe.ParentSessionID == "" && pe.HarnessSessionID != expected {
			continue // stale parent event for a different session — drop
		}
		kept = append(kept, pe) // matching parent, or any subagent event
	}
	return kept
}

// EnvLookup returns the value of key in an os.Environ()-style "K=V" slice, or ""
// if absent. The last occurrence wins (matching exec semantics). Exported so
// per-harness Profile packages — which cannot see this package's unexported
// helpers — read the launch env by exactly the same rule the hook subprocess
// does; see ConfigDirResolver.
func EnvLookup(env []string, key string) string {
	prefix := key + "="
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val = kv[len(prefix):]
		}
	}
	return val
}

# Discovery: binaries, versions & models

Before you can drive a harness you need three facts about the machine you are on: **is the CLI
installed**, **which version is it**, and **which models will it accept**. Three small packages answer
those, and they are deliberately kept apart from everything that drives a harness.

| Package | Answers | Touches the filesystem? |
|---|---|---|
| `pkg/versions` | which upstream release each adapter was last verified against | reads an embedded file only |
| `pkg/discovery` | is the binary on `PATH`, and what does `--version` say | reads and executes; never writes |
| `pkg/discovery/models` | which model ids a harness knows, offline | reads an embedded file only |
| `chat.DiscoverModels` | which models this *installed* harness offers, live | launches the harness |

## Pinned versions

```go
type Entry struct {
	Package    string // npm package, e.g. "@openai/codex"
	Binary     string // executable name on PATH
	Pinned     string // "" = not yet verified
	VerifiedAt string // YYYY-MM-DD; must be empty when Pinned is
}

func All() (map[string]Entry, error)
func Pinned(harness string) (string, bool)
func ReadFrom(path string) (map[string]Entry, error)
```

`versions.json` is **embedded**, so `All()` works under `-trimpath`, from any working directory, and
inside a container that shipped only the binary. `ReadFrom` exists for tooling that needs to read a
*different* copy — the drift checker comparing against a sibling repo's snapshot, for instance.

The pins themselves and the workflow for changing them live in [Versions & Drift](versions-drift.md).
`Pinned` returns `false` for an unknown harness *and* for a known one whose pin is empty — "not yet
verified" is not an error state, it is the honest answer for a harness whose corpus has not been
captured.

## Is it installed?

```go
type Info struct {
	Name, Harness, Binary, Path string
	Installed                   bool
	InstallHint                 string
	PinnedVersion               string
	DetectedVersion             string
	VersionMatchesPin           bool
	VersionProbeError           string
	NPMPackage                  string
}

func Lookup(name string) (Info, error)
func Discover() ([]Info, error)
func ResetCache()
```

Resolution order for `Lookup`: an exact harness key in `versions.json` → an entry whose *binary* name
matches → otherwise treat the name as a raw binary to probe on `PATH`.

Three behaviours worth knowing:

- **Not installed is not an error.** A binary missing from `PATH` returns `Installed: false`, a
  human-readable `InstallHint`, and a **nil** error. Callers that treat any error as fatal therefore
  never crash on a machine that simply lacks a harness.
- **A failed probe is not an error either.** If `--version` fails or prints nothing semver-shaped, the
  text lands in `VersionProbeError` and discovery still succeeds.
- **`VersionMatchesPin` defaults to true.** It is only set false when a pin *and* a detected version
  both exist and differ. Unknown is never reported as drift.

The probe runs `<binary> --version` and takes the first semver-shaped substring, with a generous
timeout — the node-based CLIs can take seconds to start on a loaded machine, and a tight bound was
measured killing healthy probes. Results are cached in memory keyed by **binary path + mtime**, so
repeated lookups are free while an upgrade in place invalidates the entry by itself. `ResetCache`
exists for tests.

## Which models? (offline)

```go
func KnownModels(harness string) []string
func DefaultModel(harness string) string
func IsKnownModel(harness, model string) bool
```

A curated **static** registry, embedded as JSON. It is static for a reason: neither claude-code nor
codex ships a machine-readable list-models flag — the only enumerator is the interactive `/model`
picker, and launching a CLI is far too expensive for what is usually a validation question.

`IsKnownModel` is a **helper, not a gate**. Nothing in the send path consults it; an unknown model
string is passed to the harness, which is the authority. Use it to warn, not to reject.

## Which models? (live)

```go
func DiscoverModels(ctx context.Context, opts DiscoverModelsOptions) ([]models.Info, error)

var ErrPickerUnsupported = errors.New("chat: harness does not support /model discovery")
var ErrPickerTimeout     = errors.New("chat: /model picker did not render before deadline")
```

`chat.DiscoverModels` opens a **throwaway** conversation, waits for the composer to be ready, writes
`/model`, and polls the rendered screen until the picker parses. It is read-only: it never selects a
model, and the session is discarded — the picker is simply left open as the process is torn down.

Behaviours to know:

- Only claude/claude-code and codex are supported; every other harness fails fast with
  `ErrPickerUnsupported` **before a process is launched**.
- The render budget covers the picker only — it is layered *on top of* the readiness wait, not an
  end-to-end deadline. Use `ctx` for that.
- An authentication wall surfaces as the same `ErrAuthRequired` the send path uses.
- Teardown runs on its own short, independent deadline, so a cancelled caller context still stops the
  harness.

The screen parser lives in `pkg/discovery/models` and is a deliberate byte-for-byte counterpart of the
TypeScript implementation in meta-harness: Go's regexp engine has ASCII-only shorthand classes, so the
JavaScript Unicode whitespace classes are spelled out explicitly rather than approximated. A header
gate keeps the parser from firing on an unrelated screen that happens to contain numbered rows.

## How they fit together

A launcher typically calls them in order:

1. `versions.Pinned(harness)` — what have we verified against?
2. `discovery.Lookup(harness)` — is it here, and does the installed version match?
3. `models.KnownModels(harness)` / `DefaultModel` — offer a sane model choice without spending a
   process launch.
4. Only when a user actually opens a model picker, `chat.DiscoverModels` for the live list.

Steps 1–3 are cheap and hermetic. Step 4 is the only one that starts a harness.

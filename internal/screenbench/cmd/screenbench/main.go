// screenbench replays recorded PTY byte streams through each registered
// vt100 emulator candidate and reports fidelity, stability, and
// throughput against ground-truth expected text.
//
//	screenbench --corpus test/corpus
//	screenbench --corpus test/corpus --emulator vt10x --scenario codex/short-reply
//	screenbench --corpus test/corpus --format markdown > report.md
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/internal/screenbench/emulator"
	"github.com/olesho/harness-wrapper/internal/screenbench/metrics"
	"github.com/olesho/harness-wrapper/internal/screenbench/scenario"
)

type result struct {
	Scenario     string
	Harness      string
	Emulator     string
	HasExpected  bool
	Exact        bool
	Distance     int
	NormDistance float64
	Bytes        int
	ExtractRunes int
	ExpectRunes  int
	Duration     time.Duration
	Throughput   float64 // bytes/sec
	StableAfter  time.Duration
	AllocMB      float64
	Snapshot     string
	Err          error
}

func main() {
	var (
		corpus        = flag.String("corpus", "test/corpus", "root directory of recorded scenarios")
		emuFilter     = flag.String("emulator", "", "limit to this emulator (default: all)")
		scenFilter    = flag.String("scenario", "", "limit to this scenario (path suffix match)")
		format        = flag.String("format", "table", "output format: table | markdown | json")
		dumpSnap      = flag.Bool("dump-snapshots", false, "include final snapshots in markdown output")
		writeWait     = flag.Duration("settle", 200*time.Millisecond, "post-write settling window for stability measurement")
		writeExpected = flag.Bool("write-expected", false, "bootstrap expected.txt from the selected emulator's normalized snapshot (requires --emulator)")
	)
	flag.Parse()

	scenarios, err := scenario.Discover(*corpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "screenbench: discover %s: %v\n", *corpus, err)
		os.Exit(1)
	}
	if len(scenarios) == 0 {
		fmt.Fprintf(os.Stderr, "screenbench: no scenarios found under %s\n", *corpus)
		fmt.Fprintf(os.Stderr, "  record some with `screenbench-record` first; see test/corpus/README.md\n")
		os.Exit(0)
	}

	emus := emulator.Names()
	if *emuFilter != "" {
		emus = []string{*emuFilter}
		if _, ok := emulator.Registry[*emuFilter]; !ok {
			fmt.Fprintf(os.Stderr, "screenbench: unknown emulator %q (have: %s)\n", *emuFilter, strings.Join(emulator.Names(), ", "))
			os.Exit(2)
		}
	}

	if *writeExpected && *emuFilter == "" {
		fmt.Fprintln(os.Stderr, "screenbench: --write-expected requires --emulator to pick a source")
		os.Exit(2)
	}

	var results []result
	for _, sc := range scenarios {
		if *scenFilter != "" && !strings.Contains(sc.Path, *scenFilter) && !strings.Contains(sc.Name, *scenFilter) {
			continue
		}
		for _, name := range emus {
			r := runOne(sc, name, *writeWait)
			if *writeExpected && r.Err == nil {
				p := filepath.Join(sc.Path, "expected.txt")
				if err := os.WriteFile(p, []byte(metrics.Normalize(r.Snapshot)+"\n"), 0o644); err != nil {
					fmt.Fprintf(os.Stderr, "screenbench: write %s: %v\n", p, err)
				} else {
					fmt.Fprintf(os.Stderr, "wrote %s (%d bytes, from %s)\n", p, len(metrics.Normalize(r.Snapshot)), name)
				}
			}
			results = append(results, r)
		}
	}

	switch *format {
	case "markdown":
		emitMarkdown(os.Stdout, results, *dumpSnap)
	case "json":
		emitJSON(os.Stdout, results)
	default:
		emitTable(os.Stdout, results)
	}
}

func runOne(sc *scenario.Scenario, emuName string, settle time.Duration) (r result) {
	r = result{
		Scenario:    sc.Path,
		Harness:     sc.Meta.Harness,
		Emulator:    emuName,
		HasExpected: strings.TrimSpace(sc.Expected) != "",
		Bytes:       len(sc.Bytes),
	}
	defer func() {
		if p := recover(); p != nil {
			r.Err = fmt.Errorf("panic: %v", p)
		}
	}()
	factory, ok := emulator.Registry[emuName]
	if !ok {
		r.Err = fmt.Errorf("emulator %q not registered", emuName)
		return r
	}
	emu := factory(sc.Meta.Cols, sc.Meta.Rows)

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	start := time.Now()
	if _, err := emu.Write(sc.Bytes); err != nil {
		r.Err = fmt.Errorf("write: %w", err)
		return r
	}
	r.Duration = time.Since(start)

	r.StableAfter = measureStability(emu, settle)

	runtime.ReadMemStats(&m1)
	r.AllocMB = float64(m1.TotalAlloc-m0.TotalAlloc) / (1024 * 1024)

	if r.Duration > 0 {
		r.Throughput = float64(len(sc.Bytes)) / r.Duration.Seconds()
	}

	snap := emu.Snapshot()
	r.Snapshot = snap
	r.ExtractRunes = len([]rune(metrics.Normalize(snap)))

	if r.HasExpected {
		r.ExpectRunes = len([]rune(metrics.Normalize(sc.Expected)))
		r.Exact = metrics.ExactMatch(snap, sc.Expected)
		r.Distance = metrics.Levenshtein(metrics.Normalize(snap), metrics.Normalize(sc.Expected))
		r.NormDistance = metrics.NormalizedDistance(snap, sc.Expected)
	}
	return r
}

// measureStability writes zero bytes and re-snapshots after settle to
// confirm the emulator state is quiescent. This is a placeholder until
// the bench learns to feed bytes incrementally with sleeps; for now it
// reports the settle duration if the snapshot is stable, or the time at
// which it differed.
func measureStability(emu emulator.Emulator, settle time.Duration) time.Duration {
	before := emu.Snapshot()
	deadline := time.Now().Add(settle)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if emu.Snapshot() != before {
			return time.Since(deadline.Add(-settle))
		}
	}
	return settle
}

// ---- output formatters ----

func emitTable(w io.Writer, rs []result) {
	fmt.Fprintf(w, "%-40s %-12s %-12s %8s %8s %8s %10s\n",
		"SCENARIO", "HARNESS", "EMULATOR", "BYTES", "DIST", "NDIST", "MB/s")
	for _, r := range rs {
		dist := "-"
		ndist := "-"
		if r.HasExpected {
			dist = fmt.Sprintf("%d", r.Distance)
			ndist = fmt.Sprintf("%.3f", r.NormDistance)
		}
		mbs := r.Throughput / (1024 * 1024)
		short := r.Scenario
		if len(short) > 40 {
			short = "..." + short[len(short)-37:]
		}
		errStr := ""
		if r.Err != nil {
			errStr = " ERR=" + r.Err.Error()
		}
		fmt.Fprintf(w, "%-40s %-12s %-12s %8d %8s %8s %10.2f%s\n",
			short, r.Harness, r.Emulator, r.Bytes, dist, ndist, mbs, errStr)
	}
}

func emitMarkdown(w io.Writer, rs []result, dumpSnap bool) {
	fmt.Fprintln(w, "# screenbench results")
	fmt.Fprintf(w, "\nGenerated: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	groups := map[string][]result{}
	keys := []string{}
	for _, r := range rs {
		if _, ok := groups[r.Scenario]; !ok {
			keys = append(keys, r.Scenario)
		}
		groups[r.Scenario] = append(groups[r.Scenario], r)
	}
	sort.Strings(keys)

	for _, k := range keys {
		fmt.Fprintf(w, "## %s\n\n", k)
		g := groups[k]
		if len(g) > 0 {
			fmt.Fprintf(w, "Harness: `%s` | Bytes: %d\n\n", g[0].Harness, g[0].Bytes)
		}
		fmt.Fprintln(w, "| Emulator | Exact | Distance | NDist | Extract runes | Expect runes | Time | MB/s | Alloc (MB) |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|")
		for _, r := range g {
			dist, ndist, exact := "-", "-", "n/a"
			if r.HasExpected {
				dist = fmt.Sprintf("%d", r.Distance)
				ndist = fmt.Sprintf("%.3f", r.NormDistance)
				exact = fmt.Sprintf("%v", r.Exact)
			}
			errStr := ""
			if r.Err != nil {
				errStr = " ⚠ " + r.Err.Error()
			}
			fmt.Fprintf(w, "| %s%s | %s | %s | %s | %d | %d | %s | %.2f | %.2f |\n",
				r.Emulator, errStr, exact, dist, ndist,
				r.ExtractRunes, r.ExpectRunes,
				r.Duration.Round(time.Microsecond),
				r.Throughput/(1024*1024), r.AllocMB)
		}
		fmt.Fprintln(w)
		if dumpSnap {
			for _, r := range g {
				fmt.Fprintf(w, "### %s — extracted snapshot\n\n```\n%s\n```\n\n", r.Emulator, metrics.Normalize(r.Snapshot))
			}
		}
	}
}

func emitJSON(w io.Writer, rs []result) {
	// Minimal hand-rolled JSON to avoid pulling encoding/json reflection
	// over every Snapshot string. Quote-escape just what we need.
	fmt.Fprintln(w, "[")
	for i, r := range rs {
		comma := ","
		if i == len(rs)-1 {
			comma = ""
		}
		errStr := ""
		if r.Err != nil {
			errStr = r.Err.Error()
		}
		fmt.Fprintf(w, "  {\"scenario\":%q,\"harness\":%q,\"emulator\":%q,\"bytes\":%d,\"has_expected\":%v,\"exact\":%v,\"distance\":%d,\"norm_distance\":%.4f,\"duration_ns\":%d,\"throughput_bps\":%.2f,\"alloc_mb\":%.2f,\"err\":%q}%s\n",
			r.Scenario, r.Harness, r.Emulator, r.Bytes,
			r.HasExpected, r.Exact, r.Distance, r.NormDistance,
			r.Duration.Nanoseconds(), r.Throughput, r.AllocMB, errStr, comma)
	}
	fmt.Fprintln(w, "]")
}

package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Each Session opens its own Markers on the agent's one store, so their
// mutexes guard nothing between them: two of them writing the same input id
// at once must still leave exactly one claim, never a second that silently
// replaced the first.
func TestMarkersWriteIsExclusiveAcrossInstances(t *testing.T) {
	scratch := t.TempDir()
	const writers = 8
	ms := make([]*Markers, writers)
	for i := range ms {
		m, err := OpenMarkers(scratch)
		if err != nil {
			t.Fatal(err)
		}
		ms[i] = m
	}
	for round := range 50 {
		input := fmt.Sprintf("in-%d", round)
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			won  []string
			errs []error
		)
		start := make(chan struct{})
		for i, m := range ms {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				native := fmt.Sprintf("native-%d-%d", round, i)
				err := m.Write(Marker{InputID: input, Native: native, SessionID: "s", At: time.Now()})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won = append(won, native)
				case !errors.Is(err, ErrMarked):
					errs = append(errs, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(errs) > 0 {
			t.Fatalf("round %d: %v", round, errs)
		}
		if len(won) != 1 {
			t.Fatalf("round %d: %d writers claimed %s (%v), want exactly 1", round, len(won), input, won)
		}
		mk, found, err := ms[0].Lookup(input)
		if err != nil || !found || mk.Native != won[0] {
			t.Fatalf("round %d: marker %+v found=%v err=%v, want the winner's %s", round, mk, found, err, won[0])
		}
	}
	entries, err := os.ReadDir(filepath.Join(scratch, markersDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// A ByNative miss reads the store again only when it changed: a reader asks
// about every user entry, and most are not inputs, so rereading every marker
// on each of them is quadratic. A marker another instance writes is still
// found by the next miss.
func TestMarkersByNativeRescansOnlyOnChange(t *testing.T) {
	orig := markerScanSlack
	markerScanSlack = 0 // the test's filesystem has fine timestamps
	defer func() { markerScanSlack = orig }()

	scratch := t.TempDir()
	writer, err := OpenMarkers(scratch)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenMarkers(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(Marker{InputID: "in-1", Native: "n-1", SessionID: "s", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reader.ByNative("n-1"); !ok {
		t.Fatal("n-1 not found")
	}
	scans := reader.scans
	for range 100 {
		if _, ok := reader.ByNative("tool-result-entry"); ok {
			t.Fatal("a miss was found")
		}
	}
	if reader.scans != scans {
		t.Errorf("100 misses on an unchanged store read it %d more times, want 0", reader.scans-scans)
	}

	time.Sleep(10 * time.Millisecond) // a distinct directory mtime
	if err := writer.Write(Marker{InputID: "in-2", Native: "n-2", SessionID: "s", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if mk, ok := reader.ByNative("n-2"); !ok || mk.InputID != "in-2" {
		t.Fatalf("the other instance's new marker was not found: %+v %v", mk, ok)
	}
	if reader.scans != scans+1 {
		t.Errorf("finding the new marker took %d scans, want 1", reader.scans-scans)
	}
}

// With coarse timestamps a scan cannot rule out an entry added in its own
// tick, so a store changed that close to the scan is read again on a miss.
func TestMarkersByNativeRescansARacyScan(t *testing.T) {
	scratch := t.TempDir()
	m, err := OpenMarkers(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write(Marker{InputID: "in-1", Native: "n-1", SessionID: "s", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m.ByNative("miss")
	scans := m.scans
	m.ByNative("miss")
	if m.scans != scans+1 {
		t.Errorf("a miss right after a change read the store %d times, want 1", m.scans-scans)
	}
}

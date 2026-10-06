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

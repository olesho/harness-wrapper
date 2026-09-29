package chatcore

import (
	"fmt"
	"sort"
	"sync"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// The adapter registry. This package names no harness: a harness is whatever
// adapter is registered under its name. pkg/chat registers every built-in
// adapter, so its callers resolve exactly the harnesses they always have; a
// binary that registers one harness links that harness's screen adapter and no
// other's.
var (
	registryMu sync.RWMutex
	factories  = map[string]func() turns.Adapter{}
	shared     = map[string]turns.Adapter{}
)

// RegisterAdapter makes newAdapter the source of the adapter behind every
// conversation whose Options.Harness is name: each Open and Reopen gets a new
// one. Registering a name again replaces its factory.
func RegisterAdapter(name string, newAdapter func() turns.Adapter) {
	registryMu.Lock()
	defer registryMu.Unlock()
	factories[name] = newAdapter
	delete(shared, name)
}

// resolveAdapter returns a new adapter for the harness named name.
func resolveAdapter(name string) (turns.Adapter, error) {
	registryMu.RLock()
	newAdapter := factories[name]
	registryMu.RUnlock()
	if newAdapter == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownHarness, name)
	}
	return newAdapter(), nil
}

// adapterNamed returns one adapter registered under name, shared between
// callers, or nil when none is registered. It answers questions about a
// harness that no conversation is running — which dialogs its screens paint —
// and must never drive one: a conversation gets its own from resolveAdapter.
func adapterNamed(name string) turns.Adapter {
	registryMu.RLock()
	a, ok := shared[name]
	newAdapter := factories[name]
	registryMu.RUnlock()
	if ok || newAdapter == nil {
		return a
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if a, ok := shared[name]; ok {
		return a
	}
	a = newAdapter()
	shared[name] = a
	return a
}

// registeredNames returns every registered harness name, sorted.
func registeredNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

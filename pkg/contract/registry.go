package contract

import (
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Adapter{}
)

// Register makes a available under name, the harness's name. A harness
// profile calls it from its package's init, so a runtime links a harness by
// importing its profile — one line of its harness list — and finds it with
// Lookup, never through a switch over known harnesses. Registering a name
// twice panics.
func Register(name string, a Adapter) {
	if name == "" || a == nil {
		panic("contract: Register needs a name and an adapter")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("contract: Register called twice for %q", name))
	}
	registry[name] = a
}

// Lookup returns the adapter registered under name.
func Lookup(name string) (Adapter, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	a, ok := registry[name]
	return a, ok
}

// Names lists every registered harness, sorted.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ErrAdapterMissing is the error for a harness no adapter is registered
// under: open_failed with reason adapter_missing.
func ErrAdapterMissing(name string) *Error {
	return &Error{Code: CodeOpenFailed, Reason: OpenAdapterMissing, Message: fmt.Sprintf("no adapter is registered for %q", name)}
}

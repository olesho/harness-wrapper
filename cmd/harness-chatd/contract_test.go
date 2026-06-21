package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Layer 0 of the test pyramid (see TESTING.md): freeze the outward HTTP contract
// so an accidental rename / removal / retype — which would silently break
// external clients — fails loudly here and forces a conscious golden update.
// Regenerate goldens after an INTENTIONAL change with: UPDATE_GOLDEN=1 go test ./cmd/harness-chatd/

// wireTypes is every JSON DTO on the chatd wire. Add new wire types here.
func wireTypes() []any {
	return []any{
		runTurnRequest{}, runTurnResponse{}, sessionDTO{},
		openRequest{}, openResponse{}, conversationSummary{},
		controlResponse{}, screenResponse{},
		sendRequest{}, sendResponse{}, answerRequest{},
		inputOptionDTO{}, inputRequestDTO{},
		eventDTO{}, turnDTO{}, turnEventDTO{}, historyResponse{},
		errorResponse{},
	}
}

// TestContract_WireTypes freezes each DTO's field names, json tags, and types.
func TestContract_WireTypes(t *testing.T) {
	types := wireTypes()
	sort.Slice(types, func(i, j int) bool {
		return reflect.TypeOf(types[i]).Name() < reflect.TypeOf(types[j]).Name()
	})
	var b strings.Builder
	for _, v := range types {
		rt := reflect.TypeOf(v)
		fmt.Fprintf(&b, "%s\n", rt.Name())
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			fmt.Fprintf(&b, "\t%s %s `json:%q`\n", f.Name, f.Type.String(), f.Tag.Get("json"))
		}
	}
	assertGolden(t, "wire_contract.golden", b.String())
}

// TestContract_Routes freezes the HTTP method+path surface.
func TestContract_Routes(t *testing.T) {
	s := NewServer()
	var lines []string
	for _, r := range s.routes() {
		lines = append(lines, r.method+" "+r.pattern)
	}
	sort.Strings(lines)
	assertGolden(t, "routes.golden", strings.Join(lines, "\n")+"\n")
}

// assertGolden compares got against testdata/<name>, regenerating it when
// UPDATE_GOLDEN=1. A mismatch means the frozen contract changed.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (create it with UPDATE_GOLDEN=1): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("contract drift in %s — if intentional, regenerate with UPDATE_GOLDEN=1\n--- got ---\n%s\n--- want ---\n%s", name, got, string(want))
	}
}

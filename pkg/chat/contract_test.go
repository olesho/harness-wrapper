package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Layer 0 of the test pyramid (see TESTING.md): freeze the exported pkg/chat Go
// API — the surface in-process callers depend on. A rename, removed method,
// changed field type, or altered signature fails loudly here and forces a
// conscious golden update. Regenerate after an INTENTIONAL change with:
// UPDATE_GOLDEN=1 go test ./pkg/chat/
//
// Two halves, both deliberately manual so additions are noticed:
//   - apiTypes: structs/interfaces, reflected for fields + method sets.
//   - the const/func/error block: package-level entities reflection can't
//     enumerate; listed by reference, so a removal/rename breaks compilation
//     and a value change shows in the golden.

// apiTypes is every exported type whose shape is part of the contract.
func apiTypes() []struct {
	name string
	typ  reflect.Type
} {
	return []struct {
		name string
		typ  reflect.Type
	}{
		{"Options", reflect.TypeOf(Options{})},
		{"Conversation", reflect.TypeOf(Conversation{})},
		{"Turn", reflect.TypeOf(Turn{})},
		{"Session", reflect.TypeOf(Session{})},
		{"ConversationEvent", reflect.TypeOf(ConversationEvent{})},
		{"InputRequest", reflect.TypeOf(InputRequest{})},
		{"InputOption", reflect.TypeOf(InputOption{})},
		{"InputAnswer", reflect.TypeOf(InputAnswer{})},
		{"Disposition", reflect.TypeOf(Disposition{})},
		{"InputPolicy", reflect.TypeOf(InputPolicy{})},
		{"Store", reflect.TypeOf((*Store)(nil)).Elem()},
	}
}

func TestContract_GoAPI(t *testing.T) {
	var b strings.Builder

	for _, e := range apiTypes() {
		dumpType(&b, e.name, e.typ)
	}

	// Package-level functions, typed consts, and error sentinels. Listed
	// explicitly — referencing each makes a removal a compile error here.
	b.WriteString("\n-- functions --\n")
	for _, f := range []struct {
		name string
		fn   any
	}{
		{"Open", Open},
	} {
		fmt.Fprintf(&b, "func %s %s\n", f.name, methodSig(reflect.TypeOf(f.fn), false))
	}

	b.WriteString("\n-- consts --\n")
	for _, c := range []struct {
		name string
		val  any
	}{
		{"RoleUser", RoleUser}, {"RoleAssistant", RoleAssistant}, {"RoleSystem", RoleSystem},
		{"TurnStatePending", TurnStatePending}, {"TurnStateStreaming", TurnStateStreaming},
		{"TurnStateComplete", TurnStateComplete}, {"TurnStateErrored", TurnStateErrored},
		{"EventTurn", EventTurn}, {"EventInputRequest", EventInputRequest}, {"EventInputResolved", EventInputResolved},
		{"DispositionAsk", DispositionAsk}, {"DispositionAnswer", DispositionAnswer}, {"DispositionDeny", DispositionDeny},
	} {
		fmt.Fprintf(&b, "%s %s = %q\n", c.name, reflect.TypeOf(c.val).Name(), fmt.Sprint(c.val))
	}

	b.WriteString("\n-- errors --\n")
	for _, e := range []struct {
		name string
		err  error
	}{
		{"ErrInvalidOptions", ErrInvalidOptions}, {"ErrUnknownHarness", ErrUnknownHarness},
		{"ErrNoControl", ErrNoControl}, {"ErrTurnInFlight", ErrTurnInFlight}, {"ErrClosed", ErrClosed},
		{"ErrInputPending", ErrInputPending}, {"ErrNoInputPending", ErrNoInputPending},
		{"ErrStaleInputRequest", ErrStaleInputRequest}, {"ErrUnknownOption", ErrUnknownOption},
	} {
		fmt.Fprintf(&b, "%s = %q\n", e.name, e.err.Error())
	}

	assertGolden(t, "go_api.golden", b.String())
}

// dumpType renders a type's exported fields (with json tags) and full exported
// method set in a stable, sorted form.
func dumpType(b *strings.Builder, name string, rt reflect.Type) {
	fmt.Fprintf(b, "type %s %s\n", name, rt.Kind())

	if rt.Kind() == reflect.Struct {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			if tag := f.Tag.Get("json"); tag != "" {
				fmt.Fprintf(b, "\tfield %s %s `json:%q`\n", f.Name, f.Type.String(), tag)
			} else {
				fmt.Fprintf(b, "\tfield %s %s\n", f.Name, f.Type.String())
			}
		}
	}

	var methods []string
	if rt.Kind() == reflect.Interface {
		for i := 0; i < rt.NumMethod(); i++ {
			m := rt.Method(i)
			methods = append(methods, "\tmethod "+m.Name+methodSig(m.Type, false))
		}
	} else {
		pt := reflect.PointerTo(rt)
		for i := 0; i < pt.NumMethod(); i++ {
			m := pt.Method(i)
			methods = append(methods, "\tmethod "+m.Name+methodSig(m.Type, true))
		}
	}
	sort.Strings(methods)
	for _, m := range methods {
		fmt.Fprintln(b, m)
	}
}

// methodSig renders a func type as "(in, ...) (out, out)". skipRecv drops the
// leading receiver that reflect includes for concrete-type methods (interface
// method types carry no receiver).
func methodSig(t reflect.Type, skipRecv bool) string {
	start := 0
	if skipRecv {
		start = 1
	}
	var in []string
	for i := start; i < t.NumIn(); i++ {
		if t.IsVariadic() && i == t.NumIn()-1 {
			in = append(in, "..."+t.In(i).Elem().String())
		} else {
			in = append(in, t.In(i).String())
		}
	}
	var out []string
	for i := 0; i < t.NumOut(); i++ {
		out = append(out, t.Out(i).String())
	}
	s := "(" + strings.Join(in, ", ") + ")"
	switch len(out) {
	case 0:
	case 1:
		s += " " + out[0]
	default:
		s += " (" + strings.Join(out, ", ") + ")"
	}
	return s
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

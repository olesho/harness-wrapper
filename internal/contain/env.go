//go:build linux

package contain

import (
	"slices"
	"sort"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/containment"
)

// runtimeEnv is the documented minimal allow-list a contained harness
// inherits from the caller's environment, plus every LC_* locale variable.
// Everything else is dropped unless the profile names it (authentication and
// harness-control variables) or the request's PassEnv does: a credential
// deny-list could never guarantee that unrelated secrets and socket or proxy
// variables stay out.
var runtimeEnv = []string{
	"PATH", "LANG", "LANGUAGE", "TERM", "COLORTERM", "TZ",
	"USER", "LOGNAME", "SHELL", "NO_COLOR",
}

// layout is a session's immutable state layout: the directories the child
// environment and every wrapper-side reader refer to.
type layout struct {
	Home string
	Tmp  string
	// Config is the harness's own state root (CLAUDE_CONFIG_DIR, CODEX_HOME);
	// empty for a profile without one.
	Config string
}

// lookupEnv returns the last value of key in env, exec's rule for duplicates.
func lookupEnv(env []string, key string) (string, bool) {
	val, found := "", false
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val, found = kv[len(prefix):], true
		}
	}
	return val, found
}

// childEnv builds the contained harness's environment. It returns the
// variables in name order and their names, which are all an applied policy
// records: values, credentials above all, never leave the environment.
func childEnv(m *manifest, req *containment.Request, caller []string, l layout, wd string) ([]string, []string) {
	vars := map[string]string{}
	inherit := func(name string) {
		if v, ok := lookupEnv(caller, name); ok {
			vars[name] = v
		}
	}
	for _, name := range runtimeEnv {
		inherit(name)
	}
	for _, kv := range caller {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "LC_") {
			inherit(name)
		}
	}
	for _, name := range m.AuthEnv {
		inherit(name)
	}
	for _, name := range m.ControlEnv {
		inherit(name)
	}
	for _, name := range req.PassEnv {
		inherit(name)
	}

	// Wrapper-controlled variables last, so nothing inherited can override
	// them: the private layout, the working directory and the profile's
	// fixed settings.
	for name := range vars {
		if containment.ReservedEnv(name) {
			delete(vars, name)
		}
	}
	vars["HOME"] = l.Home
	vars["TMPDIR"] = l.Tmp
	vars["TMP"] = l.Tmp
	vars["TEMP"] = l.Tmp
	vars["PWD"] = wd
	if m.State.ConfigEnv != "" && l.Config != "" {
		vars[m.State.ConfigEnv] = l.Config
	}
	for name, v := range m.Env {
		vars[name] = v
	}

	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+vars[name])
	}
	return env, slices.Clone(names)
}

package env

import (
	"sort"
	"strings"
)

// Injection-safe argv -> shell string (loomcli Part C's argvToShell discipline).
//
// Any place a prompt or user string crosses an exec boundary that is
// shell-interpreted (e.g. a containment's in-guest `env K=V <argv>` prefix), the
// argv must be reassembled with STRICT single-quoting so no metacharacter,
// newline, or leading dash can break out of its token.

// ShQuote single-quotes one argument for POSIX sh.
//
// The empty string becomes ”. Otherwise the argument is wrapped in single
// quotes and any embedded single quote is escaped via the '\” idiom
// (close-quote, escaped-quote, re-open). Nothing inside single quotes is special
// to the shell, so quotes, $, backticks, ;, newlines, and a leading - are all
// inert.
func ShQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// ArgvToShell joins an argv into a single shell-safe command string. Each token
// is independently single-quoted, so no element can inject additional tokens.
func ArgvToShell(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = ShQuote(a)
	}
	return strings.Join(parts, " ")
}

// EnvPrefixedShell builds an in-guest `env K=V … <argv>` prefix as a shell-safe
// argv-string. Both the assignments' values and the command tokens are
// single-quoted. Keys are emitted in sorted order for determinism. Used by
// containment layers whose exec transport has no dedicated env flag (design §3:
// openshell 0.0.53 exec has no --env). With no env, the plain quoted argv is
// returned — "env" with no assignments is a harmless no-op prefix, dropped when
// unused.
func EnvPrefixedShell(env map[string]string, argv []string) string {
	if len(env) == 0 {
		return ArgvToShell(argv)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, 1+len(keys)+len(argv))
	parts = append(parts, "env")
	for _, k := range keys {
		// The key itself must be a valid identifier; the value is fully quoted.
		parts = append(parts, k+"="+ShQuote(env[k]))
	}
	for _, a := range argv {
		parts = append(parts, ShQuote(a))
	}
	return strings.Join(parts, " ")
}

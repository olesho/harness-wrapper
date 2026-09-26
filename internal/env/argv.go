package env

import (
	"errors"
	"fmt"
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

// ErrInvalidEnvName is returned by EnvPrefixedShell for a key that is not a
// portable environment variable name.
var ErrInvalidEnvName = errors.New("env: invalid environment variable name")

// EnvPrefixedShell builds an in-guest `env K=V … <argv>` prefix as a shell-safe
// argv-string. Both the assignments' values and the command tokens are
// single-quoted. Keys are emitted in sorted order for determinism. Used by
// containment layers whose exec transport has no dedicated env flag (design §3:
// openshell 0.0.53 exec has no --env). With no env, the plain quoted argv is
// returned — "env" with no assignments is a harmless no-op prefix, dropped when
// unused.
//
// Keys are written unquoted, so every key must be a portable name — a letter
// or underscore, then letters, digits and underscores — or the whole command
// is refused with ErrInvalidEnvName: a key such as "X;id #" would otherwise be
// shell syntax in the command it builds.
func EnvPrefixedShell(env map[string]string, argv []string) (string, error) {
	if len(env) == 0 {
		return ArgvToShell(argv), nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if !isEnvName(k) {
			return "", fmt.Errorf("%w: %q", ErrInvalidEnvName, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, 1+len(keys)+len(argv))
	parts = append(parts, "env")
	for _, k := range keys {
		// The key is a valid identifier (checked above); the value is fully quoted.
		parts = append(parts, k+"="+ShQuote(env[k]))
	}
	for _, a := range argv {
		parts = append(parts, ShQuote(a))
	}
	return strings.Join(parts, " "), nil
}

// isEnvName reports whether name is a portable environment variable name:
// [A-Za-z_][A-Za-z0-9_]*.
func isEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_', 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z':
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

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
// argv must be reassembled with STRICT single-quoting so no metacharacter or
// newline can break out of its token.

// ShQuote single-quotes one argument for POSIX sh.
//
// The empty string becomes ”. Otherwise the argument is wrapped in single
// quotes and any embedded single quote is escaped via the '\” idiom
// (close-quote, escaped-quote, re-open). Nothing inside single quotes is special
// to the shell, so quotes, $, backticks, ;, and newlines are inert. Quoting is
// a shell-level guarantee only: a token with a leading - still reaches the
// command it is passed to as that command's option, so a caller placing a
// token where a utility parses options (e.g. `env`) must guard that itself —
// see EnvPrefixedShell.
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

// ErrInvalidEnvCommand is returned by EnvPrefixedShell when argv[0] contains
// '=': `env` would take it as one more assignment, not as the command.
var ErrInvalidEnvCommand = errors.New("env: command name contains '='")

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
//
// Shell quoting does not stop `env` itself from parsing the command: a
// command word starting with '-' would be read as an env option, and one
// containing '=' as another assignment. The assignments are therefore
// terminated with `--` (end of options for GNU coreutils, BusyBox and BSD
// env alike, all getopt-based per the POSIX utility syntax guidelines), and an
// argv[0] containing '=' — which `--` does not protect, since assignments are
// operands — is refused with ErrInvalidEnvCommand.
func EnvPrefixedShell(env map[string]string, argv []string) (string, error) {
	if len(env) == 0 {
		return ArgvToShell(argv), nil
	}
	if len(argv) > 0 && strings.ContainsRune(argv[0], '=') {
		return "", fmt.Errorf("%w: %q", ErrInvalidEnvCommand, argv[0])
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if !isEnvName(k) {
			return "", fmt.Errorf("%w: %q", ErrInvalidEnvName, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, 2+len(keys)+len(argv))
	parts = append(parts, "env")
	for _, k := range keys {
		// The key is a valid identifier (checked above); the value is fully quoted.
		parts = append(parts, k+"="+ShQuote(env[k]))
	}
	parts = append(parts, "--")
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

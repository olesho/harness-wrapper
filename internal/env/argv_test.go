package env

import (
	"errors"
	"strings"
	"testing"
)

func TestShQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "''"},
		{"plain", "hello", "'hello'"},
		{"spaces", "a b c", "'a b c'"},
		{"dollar", "$HOME", "'$HOME'"},
		{"backtick", "`id`", "'`id`'"},
		{"semicolon", "a; rm -rf /", "'a; rm -rf /'"},
		{"leading dash", "-rf", "'-rf'"},
		{"newline", "a\nb", "'a\nb'"},
		{"single quote", "it's", `'it'\''s'`},
		{"only quote", "'", `''\'''`},
		{"double quote", `he said "hi"`, `'he said "hi"'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShQuote(c.in); got != c.want {
				t.Fatalf("ShQuote(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestArgvToShell(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"empty argv", nil, ""},
		{"single", []string{"ls"}, "'ls'"},
		{"multi", []string{"echo", "hi there"}, "'echo' 'hi there'"},
		{"injection", []string{"echo", "$(rm -rf /)"}, "'echo' '$(rm -rf /)'"},
		{"embedded quote", []string{"echo", "it's"}, `'echo' 'it'\''s'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ArgvToShell(c.in); got != c.want {
				t.Fatalf("ArgvToShell(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestEnvPrefixedShell(t *testing.T) {
	t.Run("no env falls back to plain argv", func(t *testing.T) {
		got, err := EnvPrefixedShell(nil, []string{"echo", "hi"})
		want := "'echo' 'hi'"
		if err != nil || got != want {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("empty env map falls back to plain argv", func(t *testing.T) {
		got, err := EnvPrefixedShell(map[string]string{}, []string{"echo", "hi"})
		want := "'echo' 'hi'"
		if err != nil || got != want {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("keys emitted in sorted order and values quoted", func(t *testing.T) {
		env := map[string]string{"ZED": "last", "ABC": "a b", "MID": "$x"}
		got, err := EnvPrefixedShell(env, []string{"run", "cmd arg"})
		want := "env ABC='a b' MID='$x' ZED='last' -- 'run' 'cmd arg'"
		if err != nil || got != want {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("hostile value cannot break out", func(t *testing.T) {
		env := map[string]string{"K": "'; rm -rf / #"}
		got, err := EnvPrefixedShell(env, []string{"true"})
		want := `env K=''\''; rm -rf / #' -- 'true'`
		if err != nil || got != want {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, want)
		}
	})
}

// Keys are written unquoted, so a key that is not a portable name is refused:
// before, "X;touch /tmp/pwned #" became shell syntax in the command.
func TestEnvPrefixedShellRefusesInvalidKeys(t *testing.T) {
	for _, key := range []string{"", "X;touch /tmp/pwned #", "$(id)", "A B", "K=V", "1X", "-i", "K\n", "É"} {
		got, err := EnvPrefixedShell(map[string]string{"OK": "v", key: "v"}, []string{"true"})
		if !errors.Is(err, ErrInvalidEnvName) {
			t.Errorf("key %q: EnvPrefixedShell = (%q, %v), want ErrInvalidEnvName", key, got, err)
		}
		if got != "" {
			t.Errorf("key %q: EnvPrefixedShell returned a command %q with its error", key, got)
		}
	}
	got, err := EnvPrefixedShell(map[string]string{"_A1": "x", "b_2": "y"}, []string{"true"})
	if err != nil || !strings.HasPrefix(got, "env _A1='x' b_2='y' ") {
		t.Fatalf("valid keys: EnvPrefixedShell = (%q, %v)", got, err)
	}
}

// `env` parses the command word itself: a leading '-' would be an env option
// and an '=' another assignment. The prefix ends options with `--`, and an
// argv[0] containing '=' is refused.
func TestEnvPrefixedShellGuardsCommandWord(t *testing.T) {
	got, err := EnvPrefixedShell(map[string]string{"K": "v"}, []string{"-i", "x"})
	want := "env K='v' -- '-i' 'x'"
	if err != nil || got != want {
		t.Fatalf("leading dash: got (%q, %v), want (%q, nil)", got, err, want)
	}
	for _, argv0 := range []string{"A=B", "=", "./x=y"} {
		got, err := EnvPrefixedShell(map[string]string{"K": "v"}, []string{argv0, "arg"})
		if !errors.Is(err, ErrInvalidEnvCommand) || got != "" {
			t.Errorf("argv0 %q: got (%q, %v), want ErrInvalidEnvCommand", argv0, got, err)
		}
	}
	// Without env no `env` prefix is emitted, so '=' is just a quoted word.
	if got, err := EnvPrefixedShell(nil, []string{"A=B"}); err != nil || got != "'A=B'" {
		t.Errorf("no env: got (%q, %v)", got, err)
	}
}

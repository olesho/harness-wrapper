package env

import "testing"

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
		got := EnvPrefixedShell(nil, []string{"echo", "hi"})
		want := "'echo' 'hi'"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("empty env map falls back to plain argv", func(t *testing.T) {
		got := EnvPrefixedShell(map[string]string{}, []string{"echo", "hi"})
		want := "'echo' 'hi'"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("keys emitted in sorted order and values quoted", func(t *testing.T) {
		env := map[string]string{"ZED": "last", "ABC": "a b", "MID": "$x"}
		got := EnvPrefixedShell(env, []string{"run", "cmd arg"})
		want := "env ABC='a b' MID='$x' ZED='last' 'run' 'cmd arg'"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("hostile value cannot break out", func(t *testing.T) {
		env := map[string]string{"K": "'; rm -rf / #"}
		got := EnvPrefixedShell(env, []string{"true"})
		want := `env K=''\''; rm -rf / #' 'true'`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

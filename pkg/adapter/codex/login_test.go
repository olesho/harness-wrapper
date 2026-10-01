package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// lentLogin is a codex auth.json with refresh, pretty-printed as codex
// writes it: placeholder tokens, none of them a credential.
func lentLogin(t *testing.T, dir string, refresh any) string {
	t.Helper()
	b, err := json.MarshalIndent(map[string]any{
		"OPENAI_API_KEY": nil, "auth_mode": "chatgpt", "last_refresh": "2026-09-30T10:00:00Z",
		"tokens": map[string]any{
			"id_token": "eyJ.placeholder-id.sig", "access_token": "eyJ.placeholder-access.sig",
			"refresh_token": refresh, "account_id": "acct-placeholder",
		},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "lent.json")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// A lent login is written to CODEX_HOME's auth.json as the lender's codex
// wrote it, mode 0600, with a refresh token that refreshes nothing; one that
// holds a refresh token, or no access token, or is no auth.json, is refused
// and nothing is written; a new login lent later takes the old one's place.
func TestWriteLogin(t *testing.T) {
	home := t.TempDir()
	auth := filepath.Join(home, authFile)
	f := lentLogin(t, t.TempDir(), nil)
	if err := writeLogin(f, home); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(auth)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json: %v %v", fi, err)
	}
	var got map[string]any
	b, _ := os.ReadFile(auth)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	tokens, _ := got["tokens"].(map[string]any)
	if got["auth_mode"] != "chatgpt" || tokens["access_token"] != "eyJ.placeholder-access.sig" || tokens["id_token"] != "eyJ.placeholder-id.sig" ||
		tokens["account_id"] != "acct-placeholder" || tokens["refresh_token"] != withheldRefresh || got["last_refresh"] != "2026-09-30T10:00:00Z" {
		t.Errorf("auth.json %s", b)
	}

	for name, file := range map[string]string{
		"a refresh token":    lentLogin(t, t.TempDir(), "rt_the-lenders-own"),
		"no access token":    writeFile(t, `{"auth_mode":"chatgpt","tokens":{"id_token":"x"}}`),
		"not an auth.json":   writeFile(t, "sk-not-json"),
		"no file":            filepath.Join(t.TempDir(), "none"),
		"no tokens":          writeFile(t, `{"auth_mode":"chatgpt"}`),
		"a null access":      writeFile(t, `{"tokens":{"access_token":null}}`),
		"tokens not an obj.": writeFile(t, `{"tokens":"eyJ.access"}`),
	} {
		other := t.TempDir()
		err := writeLogin(file, other)
		if err == nil {
			t.Errorf("%s: written", name)
		}
		if err != nil && (strings.Contains(err.Error(), "rt_the-lenders-own") || strings.Contains(err.Error(), "sk-not-json")) {
			t.Errorf("%s: the error repeats the credential: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(other, authFile)); err == nil {
			t.Errorf("%s: an auth.json was written", name)
		}
	}

	// A rotation: the new login replaces the old one.
	newer := writeFile(t, `{"auth_mode":"chatgpt","tokens":{"access_token":"eyJ.newer.sig","refresh_token":"`+withheldRefresh+`"}}`)
	if err := writeLogin(newer, home); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(auth); !strings.Contains(string(b), "eyJ.newer.sig") || strings.Contains(string(b), "placeholder-access") {
		t.Errorf("after a rotation auth.json is %s", b)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 1 {
		t.Errorf("CODEX_HOME holds %v", ents)
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// An agent on a lent login keeps codex's credentials in auth.json; every
// other kind keeps them in memory only.
func TestProvisionLentLogin(t *testing.T) {
	for kind, store := range map[string]string{
		CredentialLogin: `cli_auth_credentials_store = "file"`, CredentialAPIKey: `cli_auth_credentials_store = "ephemeral"`,
		CredentialAccessToken: `cli_auth_credentials_store = "ephemeral"`,
	} {
		s := spec()
		s.Credential = &contract.CredentialRef{Kind: kind}
		res, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/codex", Layout: layout(), Spec: s})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		for _, f := range res.Files {
			if f.Root == contract.RootConfig && f.Path == "config.toml" {
				toml := string(f.Content())
				if !strings.Contains(toml, store) || strings.Count(toml, "cli_auth_credentials_store") != 1 {
					t.Errorf("%s: config.toml\n%s", kind, toml)
				}
				if i, j := strings.Index(toml, "cli_auth_credentials_store"), strings.Index(toml, "["); j < i {
					t.Errorf("%s: the store is not a top-level key\n%s", kind, toml)
				}
			}
		}
		if len(res.SecretPaths) != 1 || res.SecretPaths[0] != (contract.RootPath{Root: contract.RootConfig, Path: authFile}) {
			t.Errorf("%s: secret paths %v", kind, res.SecretPaths)
		}
	}
}

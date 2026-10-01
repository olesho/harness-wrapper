package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// withheldRefresh stands in a lent login's auth.json where the lender's
// refresh token was. A refresh token is spent when it is used, so a copy that
// refreshed would log the lender out; this one refreshes nothing, and codex
// runs on the access token until it expires.
const withheldRefresh = "withheld-from-a-lent-login"

// maxLogin bounds a lent login's file.
const maxLogin = 64 << 10

// writeLogin writes the ChatGPT login lent in file (CredentialLogin) to
// home's auth.json, codex's credential store for it: the login as the
// lender's codex wrote it, with withheldRefresh where its refresh token was.
// A login that holds a refresh token is refused, never written down, and so
// is one with no access token. The file is replaced whole, mode 0600, so a
// new login lent by a credential rotation takes the old one's place.
func writeLogin(file, home string) error {
	f, err := os.Open(file) //nolint:gosec // the staged credential's path
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return fmt.Errorf("credential file: %w", pe.Err)
		}
		return errors.New("credential file unreadable")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxLogin+1))
	_ = f.Close()
	if err != nil || len(b) > maxLogin {
		return fmt.Errorf("credential file unreadable, or over %d bytes", maxLogin)
	}
	// What the file holds is a credential: no error repeats any of it.
	var login map[string]any
	if json.Unmarshal(b, &login) != nil {
		return errors.New("credential file is not a codex auth.json")
	}
	tokens, _ := login["tokens"].(map[string]any)
	if access, _ := tokens["access_token"].(string); access == "" {
		return errors.New("the lent login holds no access token")
	}
	if refresh, _ := tokens["refresh_token"].(string); refresh != "" && refresh != withheldRefresh {
		return errors.New("the lent login holds a refresh token: lend its access token alone " +
			"(a refresh token is spent when it is used, and a copy that refreshed would log the lender out)")
	}
	tokens["refresh_token"] = withheldRefresh
	out, err := json.Marshal(login)
	if err != nil {
		return err
	}
	return writeWhole(filepath.Join(home, authFile), out)
}

// writeWhole replaces path with data, mode 0600: a temporary file beside it,
// synced, then renamed over it.
func writeWhole(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

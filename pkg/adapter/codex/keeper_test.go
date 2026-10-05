package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// fakeServerEnv makes the test binary, started as "codex app-server", the
// fake app-server of the keeper's tests.
const fakeServerEnv = "HW_FAKE_CODEX_APP_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) == "1" && len(os.Args) > 1 && os.Args[1] == "app-server" {
		fakeAppServer(os.Stdin, os.Stdout, os.Getenv("CODEX_HOME"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAppServer speaks the keeper's part of codex's app-server: a
// device-code sign-in, which a file approve-<code> (or deny-<code>) in the
// home completes; a refresh, which rotates the login's tokens unless a file
// revoked is in the home; and a sign-out.
func fakeAppServer(in io.Reader, out io.Writer, home string) {
	var wmu sync.Mutex
	send := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, '\n'))
	}
	answer := func(id json.RawMessage, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	refuse := func(id json.RawMessage, code int, msg string) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	}
	logins := 0
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		switch m.Method {
		case "initialize":
			answer(m.ID, map[string]string{"userAgent": "fake-codex"})
		case "account/login/start":
			logins++
			id, code := fmt.Sprintf("login-%d", logins), fmt.Sprintf("CODE-%d", logins)
			answer(m.ID, map[string]string{"type": "chatgptDeviceCode", "loginId": id, "userCode": code, "verificationUrl": "https://auth.fake.example/codex/device"})
			go func() {
				for range 400 {
					if _, err := os.Stat(filepath.Join(home, "approve-"+code)); err == nil {
						writeFakeAuth(home, 1, time.Hour)
						send(map[string]any{"jsonrpc": "2.0", "method": "account/login/completed", "params": map[string]any{"loginId": id, "success": true}})
						return
					}
					if _, err := os.Stat(filepath.Join(home, "deny-"+code)); err == nil {
						send(map[string]any{"jsonrpc": "2.0", "method": "account/login/completed", "params": map[string]any{"loginId": id, "success": false, "error": "denied"}})
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
			}()
		case "account/login/cancel":
			answer(m.ID, map[string]any{})
		case "account/read":
			b, err := os.ReadFile(filepath.Join(home, authFile))
			if err != nil {
				answer(m.ID, map[string]any{"account": nil, "requiresOpenaiAuth": true})
				continue
			}
			if _, err := os.Stat(filepath.Join(home, "revoked")); err == nil {
				refuse(m.ID, -32000, "refresh_token_invalidated")
				continue
			}
			var a struct {
				Tokens struct {
					Refresh string `json:"refresh_token"`
				} `json:"tokens"`
			}
			_ = json.Unmarshal(b, &a)
			gen := 1
			_, _ = fmt.Sscanf(a.Tokens.Refresh, "rt-%d", &gen)
			writeFakeAuth(home, gen+1, time.Hour)
			answer(m.ID, map[string]any{"account": map[string]string{"type": "chatgpt", "email": "keeper@example.com"}, "requiresOpenaiAuth": true})
		case "account/logout":
			_ = os.Remove(filepath.Join(home, authFile))
			answer(m.ID, map[string]any{})
		default:
			refuse(m.ID, -32601, "unknown method "+m.Method)
		}
	}
}

// writeFakeAuth writes codex's auth.json for generation gen of a login whose
// access token lasts life: what the fake app-server keeps.
func writeFakeAuth(home string, gen int, life time.Duration) {
	exp := time.Now().Add(life).Unix()
	b, _ := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil, "auth_mode": "chatgpt", "last_refresh": time.Now().UTC().Format(time.RFC3339),
		"tokens": map[string]any{
			"id_token":      jwt(map[string]any{"email": "keeper@example.com", "exp": exp, authClaim: planClaims}),
			"access_token":  jwt(map[string]any{"exp": exp, "gen": gen, authClaim: planClaims}),
			"refresh_token": fmt.Sprintf("rt-%d", gen), "account_id": "acct-1",
		},
	})
	_ = os.WriteFile(filepath.Join(home, authFile), b, 0o600)
}

// fakeKeeper is a keeper over a temporary home, its codex the fake
// app-server.
func fakeKeeper(t *testing.T) (contract.Keeper, string) {
	t.Helper()
	t.Setenv(fakeServerEnv, "1")
	t.Setenv(adapter.HarnessEnvVar, fakeServerEnv)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := distribution(t, self)
	home := filepath.Join(t.TempDir(), "keeper")
	k, err := lookup(t).Keep(contract.KeeperRequest{Contract: contract.Version, HarnessRoot: root, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	return k, home
}

func approve(t interface{ Errorf(string, ...any) }, home, code string) {
	if err := os.WriteFile(filepath.Join(home, "approve-"+code), nil, 0o600); err != nil {
		t.Errorf("approving: %v", err)
	}
}

// codex's keeper passes the kit's keeper scenario, its codex the fake
// app-server.
func TestKeeperConforms(t *testing.T) {
	t.Setenv(fakeServerEnv, "1")
	t.Setenv(adapter.HarnessEnvVar, fakeServerEnv)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	skip := map[string]string{}
	for _, s := range conformance.Scenarios() {
		if s != "describe" && s != "keeper" {
			skip[s] = "the fake app-server serves the keeper alone"
		}
	}
	conformance.Run(conformance.Testing(t), conformance.Fixture{
		Adapter: adapter.New(Profile{}), HarnessRoot: distribution(t, self), Skip: skip, Timeout: 10 * time.Second,
		Approve: func(t conformance.T, k contract.Keeper, dc contract.DeviceCode) {
			approve(t, k.(*keeper).home, dc.Code)
		},
	})
}

// The keeper lends the login with no refresh token, which stays in its home;
// a refresh rotates both tokens; the lent login has a placeholder.
func TestKeeperLendsNoRefreshToken(t *testing.T) {
	k, home := fakeKeeper(t)
	ctx := context.Background()
	dc, err := k.SignIn(ctx)
	if err != nil || dc.Code == "" || !strings.HasPrefix(dc.URL, "https://") {
		t.Fatalf("SignIn: %+v %v", dc, err)
	}
	approve(t, home, dc.Code)
	waitState(t, k, contract.LoginSignedIn)
	st, _ := k.Status(ctx)
	if st.Account != "keeper@example.com" || st.Plan != "plus" || st.Expiry == nil {
		t.Errorf("status %+v", st)
	}
	lent, err := k.Lend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(lent.Credential), "rt-") || strings.Contains(string(lent.Credential), "refresh_token") {
		t.Errorf("the lent login holds a refresh token: %s", lent.Credential)
	}
	if b, _ := os.ReadFile(filepath.Join(home, authFile)); !strings.Contains(string(b), `"rt-1"`) {
		t.Error("the keeper's own login lost its refresh token")
	}
	if fmt.Sprint(lent) != lent.String() || strings.Contains(lent.String(), "eyJ") {
		t.Errorf("a lent login prints its credential: %v", lent)
	}
	if _, err := lookup(t).Placeholder(contract.PlaceholderRequest{Contract: contract.Version, Kind: CredentialLogin, Credential: lent.Credential, Nonce: nonce}); err != nil {
		t.Errorf("the lent login has no placeholder: %v", err)
	}
	if err := k.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := k.Lend(ctx)
	if err != nil || string(again.Credential) == string(lent.Credential) {
		t.Errorf("a refresh lent the same login (%v)", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, authFile)); !strings.Contains(string(b), `"rt-2"`) {
		t.Error("the refresh did not rotate the refresh token")
	}
}

// A login whose refresh the service refuses says so, and once its access
// token expires, it is expired; a denied sign-in leaves the keeper signed
// out, saying why.
func TestKeeperRefusals(t *testing.T) {
	k, home := fakeKeeper(t)
	ctx := context.Background()
	dc, err := k.SignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "deny-"+dc.Code), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, k, contract.LoginSignedOut)
	if !strings.Contains(st.Error, "denied") {
		t.Errorf("a denied sign-in: %+v", st)
	}
	if err := k.Refresh(ctx); codeOf(err) != contract.CodeUnexpected {
		t.Errorf("Refresh with no login: %v, want unexpected", err)
	}

	writeFakeAuth(home, 3, -time.Minute)
	if err := os.WriteFile(filepath.Join(home, "revoked"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := k.Refresh(ctx); codeOf(err) != contract.CodeInternal {
		t.Errorf("a refused refresh: %v, want internal", err)
	}
	st, _ = k.Status(ctx)
	if st.State != contract.LoginExpired || !strings.Contains(st.Error, "refresh_token_invalidated") {
		t.Errorf("an expired login whose refresh was refused: %+v", st)
	}
	if err := k.SignOut(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := k.Status(ctx); st.State != contract.LoginSignedOut {
		t.Errorf("after SignOut: %+v", st)
	}
}

func waitState(t *testing.T, k contract.Keeper, want contract.LoginState) contract.LoginStatus {
	t.Helper()
	var st contract.LoginStatus
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		var err error
		if st, err = k.Status(context.Background()); err == nil && st.State == want {
			return st
		}
	}
	t.Fatalf("the keeper is %+v, want %s", st, want)
	return st
}

func codeOf(err error) contract.Code {
	var e *contract.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// The keeper against the real ChatGPT, with the pinned codex HW_REAL_CODEX
// names, when HW_KEEPER_LIVE=1: it prints a device code for a person to
// approve, waits for the sign-in, refreshes the login once and signs out. It
// is a separate ChatGPT session; the person's other logins are untouched.
func TestCodexKeeperLive(t *testing.T) {
	if os.Getenv("HW_KEEPER_LIVE") != "1" {
		t.Skip("HW_KEEPER_LIVE is not 1: a live sign-in needs a person to approve it")
	}
	bin := realCodex(t)
	home := filepath.Join(t.TempDir(), "keeper")
	k, err := lookup(t).Keep(contract.KeeperRequest{Contract: contract.Version, HarnessRoot: distribution(t, bin), Home: home})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	ctx := context.Background()
	dc, err := k.SignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("\n  open %s and enter %s\n\n", dc.URL, dc.Code)
	deadline := time.Now().Add(10 * time.Minute)
	for {
		st, err := k.Status(ctx)
		if err == nil && st.State == contract.LoginSignedIn {
			t.Logf("signed in, plan %s, expires %s", st.Plan, st.Expiry)
			break
		}
		if err == nil && st.State == contract.LoginSignedOut {
			t.Fatalf("the sign-in ended signed out: %s", st.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("not approved within 10 minutes")
		}
		time.Sleep(2 * time.Second)
	}
	first, err := k.Lend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	second, err := k.Lend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("refreshed: lent login changed %v, expiry %s → %s", string(first.Credential) != string(second.Credential), first.Expiry, second.Expiry)
	if second.Expiry.Before(first.Expiry) {
		t.Errorf("the refreshed login expires earlier")
	}
	if err := k.SignOut(ctx); err != nil {
		t.Errorf("SignOut: %v", err)
	}
}

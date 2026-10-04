package fakeadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// KeeperURL is where the fake's device-code sign-in sends its user.
const KeeperURL = "https://login.fake.example/device"

// loginFile is where the fake keeps its login, in the keeper's home.
const loginFile = "fake-login.json"

// login is the fake's kept login: a token, which a refresh replaces.
type login struct {
	Generation int       `json:"generation"`
	Token      string    `json:"token"`
	Expiry     time.Time `json:"expiry"`
}

// LoginLife is how long a fake login's token lasts.
const LoginLife = time.Hour

// keeper is the fake's login keeper: the login a file in its home, a sign-in
// pending in memory until Approve.
type keeper struct {
	a    *Adapter
	home string

	mu      sync.Mutex
	pending string // the code of the sign-in under way
}

// Keep opens the fake's keeper over req.Home.
func (a *Adapter) Keep(req contract.KeeperRequest) (contract.Keeper, error) {
	if !contract.Compatible(req.Contract) {
		return nil, contract.Errorf(contract.CodeProtocol, "contract %q", req.Contract)
	}
	if !filepath.IsAbs(req.Home) || filepath.Clean(req.Home) != req.Home {
		return nil, &contract.Error{Code: contract.CodeProtocol, Field: "home", Message: "not a clean absolute path"}
	}
	if a.breaks("keeper-forgets") {
		_ = os.Remove(filepath.Join(req.Home, loginFile))
	}
	return &keeper{a: a, home: req.Home}, nil
}

// Approve completes the fake keeper's sign-in under way with code, as its
// user would on the device-code page.
func Approve(k contract.Keeper, code string) error {
	fk, ok := k.(*keeper)
	if !ok {
		return errors.New("not the fake's keeper")
	}
	fk.mu.Lock()
	defer fk.mu.Unlock()
	if fk.pending == "" || fk.pending != code {
		return errors.New("no sign-in under way with that code")
	}
	fk.pending = ""
	return fk.write(login{Generation: 1, Token: "fake-token-1-" + code, Expiry: time.Now().Add(LoginLife)})
}

func (k *keeper) read() (login, bool, error) {
	b, err := os.ReadFile(filepath.Join(k.home, loginFile))
	if errors.Is(err, os.ErrNotExist) {
		return login{}, false, nil
	}
	if err != nil {
		return login{}, false, err
	}
	var l login
	if err := json.Unmarshal(b, &l); err != nil {
		return login{}, false, err
	}
	return l, true, nil
}

func (k *keeper) write(l login) error {
	b, _ := json.Marshal(l)
	if err := os.MkdirAll(k.home, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(k.home, loginFile), b, 0o600)
}

func (k *keeper) SignIn(context.Context) (contract.DeviceCode, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return contract.DeviceCode{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pending = hex.EncodeToString(b[:])
	return contract.DeviceCode{URL: KeeperURL, Code: k.pending}, nil
}

func (k *keeper) Status(context.Context) (contract.LoginStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending != "" {
		return contract.LoginStatus{State: contract.LoginSigningIn}, nil
	}
	l, ok, err := k.read()
	if err != nil {
		return contract.LoginStatus{}, contract.Errorf(contract.CodeInternal, "the fake's login: %v", err)
	}
	if !ok {
		return contract.LoginStatus{State: contract.LoginSignedOut}, nil
	}
	st := contract.LoginStatus{State: contract.LoginSignedIn, Account: "fake@example.com", Plan: "fake", Expiry: &l.Expiry}
	if time.Now().After(l.Expiry) {
		st.State = contract.LoginExpired
	}
	return st, nil
}

func (k *keeper) Refresh(context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok, err := k.read()
	if err != nil || !ok {
		return contract.Errorf(contract.CodeUnexpected, "no login to refresh")
	}
	l.Generation++
	l.Token = fmt.Sprintf("fake-token-%d-%s", l.Generation, hex.EncodeToString([]byte(l.Token))[:8])
	l.Expiry = time.Now().Add(LoginLife)
	return k.write(l)
}

func (k *keeper) Lend(context.Context) (contract.Lent, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok, err := k.read()
	if err != nil || !ok {
		return contract.Lent{}, contract.Errorf(contract.CodeUnexpected, "no login to lend")
	}
	tok := l.Token
	if k.a.breaks("keeper-lends-unbrokerable") {
		tok += "\nrefresh-" + tok
	}
	return contract.Lent{Credential: []byte(tok), Expiry: l.Expiry}, nil
}

func (k *keeper) SignOut(context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pending = ""
	if k.a.breaks("keeper-signout-keeps") {
		return nil
	}
	if err := os.Remove(filepath.Join(k.home, loginFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (k *keeper) Close() error { return nil }

package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/internal/procgroup"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// The login keeper (capability login_keeper) keeps a ChatGPT login for a
// runtime, outside every agent: the official codex, signed in once with a
// device code, holds the login in its home — CODEX_HOME, with codex's
// credential store a file — and refreshes it itself when asked, rotating its
// refresh token, which never leaves the home. The keeper lends the login as
// a CredentialLogin with no refresh token, which Placeholder takes. It drives
// codex's app-server, the protocol a Session speaks, only for a sign-in, a
// refresh and a sign-out, and reads the login from auth.json otherwise.

// keeperConfig is the keeper's config.toml: the login in auth.json.
const keeperConfig = `# The runtime's login keeper: codex keeps the login in auth.json here.
cli_auth_credentials_store = "file"

[analytics]
enabled = false
`

// keeperCallWait bounds one app-server call of the keeper's.
const keeperCallWait = time.Minute

// Keep opens the login keeper over req.Home, codex's CODEX_HOME for it.
func (Profile) Keep(req contract.KeeperRequest) (contract.Keeper, error) {
	if err := os.MkdirAll(req.Home, 0o700); err != nil {
		return nil, contract.Errorf(contract.CodeInternal, "the keeper's home: %v", err)
	}
	if err := writeWhole(filepath.Join(req.Home, configFile), []byte(keeperConfig)); err != nil {
		return nil, contract.Errorf(contract.CodeInternal, "the keeper's config: %v", err)
	}
	return &keeper{bin: BinaryPath(req.HarnessRoot), home: req.Home}, nil
}

// keeper is codex's login keeper. op serializes its operations; mu guards
// what codex's notifications change, so that a notification never waits on
// an operation waiting on codex.
type keeper struct {
	bin, home string

	op  sync.Mutex
	srv *appServer // while an operation or a sign-in needs codex

	mu      sync.Mutex
	pending string // the sign-in under way, by codex's login id
	lastErr string // why the last sign-in or refresh failed
}

func (k *keeper) authPath() string { return filepath.Join(k.home, authFile) }

// ensure starts codex's app-server, unless it runs.
func (k *keeper) ensure(ctx context.Context) (*appServer, error) {
	if k.srv != nil && !k.srv.exited() {
		return k.srv, nil
	}
	srv, err := startAppServer(ctx, k.bin, k.home, k.onNote)
	if err != nil {
		return nil, contract.Errorf(contract.CodeInternal, "codex app-server: %v", err)
	}
	k.srv = srv
	return srv, nil
}

// release stops codex's app-server unless a sign-in waits on it.
func (k *keeper) release() {
	k.mu.Lock()
	waiting := k.pending != ""
	k.mu.Unlock()
	if !waiting && k.srv != nil {
		k.srv.close()
		k.srv = nil
	}
}

// onNote takes codex's notifications: the end of a sign-in.
func (k *keeper) onNote(method string, params json.RawMessage) {
	if method != "account/login/completed" {
		return
	}
	var done struct {
		LoginID string  `json:"loginId"`
		Success bool    `json:"success"`
		Error   *string `json:"error"`
	}
	if json.Unmarshal(params, &done) != nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if done.LoginID != "" && done.LoginID != k.pending {
		return
	}
	k.pending = ""
	k.lastErr = ""
	if !done.Success {
		k.lastErr = "the sign-in failed"
		if done.Error != nil && *done.Error != "" {
			k.lastErr = "the sign-in failed: " + *done.Error
		}
	}
}

func (k *keeper) SignIn(ctx context.Context) (contract.DeviceCode, error) {
	k.op.Lock()
	defer k.op.Unlock()
	srv, err := k.ensure(ctx)
	if err != nil {
		return contract.DeviceCode{}, err
	}
	k.mu.Lock()
	prev := k.pending
	k.pending = ""
	k.mu.Unlock()
	if prev != "" {
		_, _ = srv.call(ctx, "account/login/cancel", map[string]string{"loginId": prev})
	}
	raw, err := srv.call(ctx, "account/login/start", map[string]string{"type": "chatgptDeviceCode"})
	if err != nil {
		k.release()
		return contract.DeviceCode{}, contract.Errorf(contract.CodeInternal, "codex refused the sign-in: %v", err)
	}
	var started struct {
		LoginID         string `json:"loginId"`
		UserCode        string `json:"userCode"`
		VerificationURL string `json:"verificationUrl"`
	}
	if json.Unmarshal(raw, &started) != nil || started.LoginID == "" || started.UserCode == "" || started.VerificationURL == "" {
		k.release()
		return contract.DeviceCode{}, contract.Errorf(contract.CodeInternal, "codex answered the sign-in with no device code")
	}
	k.mu.Lock()
	k.pending, k.lastErr = started.LoginID, ""
	k.mu.Unlock()
	return contract.DeviceCode{URL: started.VerificationURL, Code: started.UserCode}, nil
}

func (k *keeper) Status(context.Context) (contract.LoginStatus, error) {
	k.mu.Lock()
	pending, lastErr := k.pending, k.lastErr
	k.mu.Unlock()
	if pending != "" {
		return contract.LoginStatus{State: contract.LoginSigningIn}, nil
	}
	l, err := k.read()
	if errors.Is(err, fs.ErrNotExist) {
		return contract.LoginStatus{State: contract.LoginSignedOut, Error: lastErr}, nil
	}
	if err != nil {
		return contract.LoginStatus{}, err
	}
	st := contract.LoginStatus{State: contract.LoginSignedIn, Account: l.account, Plan: l.plan, Error: lastErr}
	if !l.expiry.IsZero() {
		exp := l.expiry
		st.Expiry = &exp
		if !time.Now().Before(exp) {
			st.State = contract.LoginExpired
		}
	}
	return st, nil
}

func (k *keeper) Refresh(ctx context.Context) error {
	k.op.Lock()
	defer k.op.Unlock()
	if _, err := k.read(); err != nil {
		return contract.Errorf(contract.CodeUnexpected, "no login to refresh")
	}
	srv, err := k.ensure(ctx)
	if err != nil {
		return err
	}
	defer k.release()
	raw, err := srv.call(ctx, "account/read", map[string]bool{"refreshToken": true})
	var acct struct {
		Account *json.RawMessage `json:"account"`
	}
	switch {
	case err != nil:
		err = fmt.Errorf("codex could not refresh the login: %w", err)
	case json.Unmarshal(raw, &acct) != nil || acct.Account == nil || string(*acct.Account) == "null":
		err = errors.New("codex holds no login after the refresh")
	}
	k.mu.Lock()
	k.lastErr = ""
	if err != nil {
		k.lastErr = err.Error()
	}
	k.mu.Unlock()
	if err != nil {
		return contract.Errorf(contract.CodeInternal, "%v", err)
	}
	return nil
}

func (k *keeper) Lend(context.Context) (contract.Lent, error) {
	b, err := os.ReadFile(k.authPath())
	if err != nil {
		return contract.Lent{}, contract.Errorf(contract.CodeUnexpected, "no login to lend")
	}
	var login map[string]any
	if json.Unmarshal(b, &login) != nil {
		return contract.Lent{}, contract.Errorf(contract.CodeInternal, "codex's auth.json is not JSON")
	}
	tokens, _ := login["tokens"].(map[string]any)
	access, _ := tokens["access_token"].(string)
	if access == "" {
		return contract.Lent{}, contract.Errorf(contract.CodeUnexpected, "the login holds no access token")
	}
	// What refreshes the login stays in the keeper's home.
	delete(tokens, "refresh_token")
	out, err := json.Marshal(login)
	if err != nil {
		return contract.Lent{}, err
	}
	var exp time.Time
	if c, ok := jwtClaims(access); ok {
		if e, ok := c["exp"].(float64); ok {
			exp = time.Unix(int64(e), 0)
		}
	}
	return contract.Lent{Credential: out, Expiry: exp}, nil
}

func (k *keeper) SignOut(ctx context.Context) error {
	k.op.Lock()
	defer k.op.Unlock()
	k.mu.Lock()
	prev := k.pending
	k.pending, k.lastErr = "", ""
	k.mu.Unlock()
	if srv, err := k.ensure(ctx); err == nil {
		if prev != "" {
			_, _ = srv.call(ctx, "account/login/cancel", map[string]string{"loginId": prev})
		}
		_, _ = srv.call(ctx, "account/logout", nil)
		k.release()
	}
	if err := os.Remove(k.authPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return contract.Errorf(contract.CodeInternal, "forgetting the login: %v", err)
	}
	return nil
}

func (k *keeper) Close() error {
	k.op.Lock()
	defer k.op.Unlock()
	k.mu.Lock()
	k.pending = ""
	k.mu.Unlock()
	k.release()
	return nil
}

// keptLogin is what the keeper reads of codex's auth.json: no secret.
type keptLogin struct {
	account, plan string
	expiry        time.Time
}

func (k *keeper) read() (keptLogin, error) {
	b, err := os.ReadFile(k.authPath())
	if err != nil {
		return keptLogin{}, err
	}
	var login struct {
		Tokens struct {
			IDToken     string `json:"id_token"`
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &login) != nil || login.Tokens.AccessToken == "" {
		return keptLogin{}, contract.Errorf(contract.CodeInternal, "codex's auth.json holds no ChatGPT login")
	}
	var l keptLogin
	if c, ok := jwtClaims(login.Tokens.AccessToken); ok {
		if e, ok := c["exp"].(float64); ok {
			l.expiry = time.Unix(int64(e), 0)
		}
	}
	if c, ok := jwtClaims(login.Tokens.IDToken); ok {
		l.account, _ = c["email"].(string)
		if a, ok := c[authClaim].(map[string]any); ok {
			l.plan, _ = a["chatgpt_plan_type"].(string)
		}
	}
	return l, nil
}

// appServer is a codex app-server the keeper speaks JSON-RPC to.
type appServer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *tailBuffer
	onNote func(method string, params json.RawMessage)
	done   chan struct{}

	wmu   sync.Mutex
	cmu   sync.Mutex
	next  int64
	calls map[int64]chan rpcResult
}

// startAppServer starts codex's app-server over home and initializes it.
func startAppServer(ctx context.Context, bin, home string, onNote func(string, json.RawMessage)) (*appServer, error) {
	cmd := exec.Command(bin, "app-server")
	cmd.Dir = home
	cmd.Env = append(adapter.HostEnv(), "HOME="+home, "CODEX_HOME="+home)
	procgroup.Set(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		_, _, _, _ = outR.Close(), outW.Close(), errR.Close(), errW.Close()
		return nil, err
	}
	_, _ = outW.Close(), errW.Close()
	s := &appServer{cmd: cmd, stdin: stdin, stderr: newTailBuffer(stderrTail), onNote: onNote, done: make(chan struct{}), calls: map[int64]chan rpcResult{}}
	go func() {
		_, _ = io.Copy(s.stderr, errR)
		_ = errR.Close()
	}()
	go s.read(outR)
	ictx, cancel := context.WithTimeout(ctx, initWait)
	defer cancel()
	if _, err := s.call(ictx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "harness-wrapper-keeper", "title": "harness-wrapper keeper", "version": adapter.Name()},
	}); err != nil {
		s.close()
		if tail := lastLine(s.stderr.String()); tail != "" {
			return nil, fmt.Errorf("initialize: %w: %s", err, tail)
		}
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := s.write(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *appServer) read(r io.ReadCloser) {
	defer func() {
		_ = r.Close()
		_ = s.cmd.Wait()
		s.cmu.Lock()
		for id, ch := range s.calls {
			ch <- rpcResult{err: errExited}
			delete(s.calls, id)
		}
		s.cmu.Unlock()
		close(s.done)
	}()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readBoundedLine(br, frameMax)
		if len(bytes.TrimSpace(line)) > 0 {
			var m message
			if json.Unmarshal(line, &m) == nil {
				s.dispatch(&m)
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *appServer) dispatch(m *message) {
	switch {
	case m.Method != "" && len(m.ID) > 0:
		// A request of codex's own: none is served here.
		_ = s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "not supported by this host"}})
	case m.Method != "":
		s.onNote(m.Method, m.Params)
	case len(m.ID) > 0:
		var id int64
		if json.Unmarshal(m.ID, &id) != nil {
			return
		}
		s.cmu.Lock()
		ch := s.calls[id]
		delete(s.calls, id)
		s.cmu.Unlock()
		if ch == nil {
			return
		}
		if m.Error != nil {
			ch <- rpcResult{err: m.Error}
			return
		}
		ch <- rpcResult{result: m.Result}
	}
}

func (s *appServer) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (s *appServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, keeperCallWait)
	defer cancel()
	s.cmu.Lock()
	s.next++
	id := s.next
	ch := make(chan rpcResult, 1)
	s.calls[id] = ch
	s.cmu.Unlock()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := s.write(msg); err != nil {
		s.cmu.Lock()
		delete(s.calls, id)
		s.cmu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		s.cmu.Lock()
		delete(s.calls, id)
		s.cmu.Unlock()
		return nil, ctx.Err()
	}
}

func (s *appServer) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// close ends the app-server: its stdin closed, then its group signalled if
// it lingers.
func (s *appServer) close() {
	s.wmu.Lock()
	_ = s.stdin.Close()
	s.wmu.Unlock()
	select {
	case <-s.done:
	case <-time.After(quitWait):
		procgroup.Signal(s.cmd, true)
		<-s.done
	}
}

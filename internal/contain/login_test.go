package contain

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestLoginAllowedBeforeActivation: sessions of a built-in profile that is not
// activated yet (codex) are refused, sessions of an activated one
// (claude-code) are not, and both login flows are available.
func TestLoginAllowedBeforeActivation(t *testing.T) {
	for _, h := range []string{"claude", "claude-code"} {
		if _, _, err := ProfileID(h); err != nil {
			t.Errorf("%s: the activated profile refused a session: %v", h, err)
		}
	}
	if _, _, err := ProfileID("codex"); !isRefusal(err, StageProfile) || !strings.Contains(err.Error(), "not activated") {
		t.Errorf("codex: sessions of an inactive profile must be refused at the profile stage, got %v", err)
	}
	for _, h := range []string{"claude", "claude-code", "codex"} {
		if _, err := LoginFlowFor(h); err != nil {
			t.Errorf("%s: login flow: %v", h, err)
		}
	}
	if _, err := LoginFlowFor("opencode"); !isRefusal(err, StageProfile) {
		t.Errorf("a harness without a profile has no login flow, got %v", err)
	}
	restore := RegisterTestProfile(TestProfile{Harness: "no-login"})
	defer restore()
	if _, err := LoginFlowFor("no-login"); !isRefusal(err, StageProfile) || !strings.Contains(err.Error(), "no login flow") {
		t.Errorf("a profile without a login flow must refuse a login, got %v", err)
	}
}

// TestBuiltinLoginFlows pins the login and status commands and checks the
// patterns against the pinned versions' output: sign-in pages and codes as
// they print them (values changed), and their status reports.
func TestBuiltinLoginFlows(t *testing.T) {
	claude, err := LoginFlowFor("claude")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(claude.Args, []string{"auth", "login"}) || !slices.Equal(claude.Status, []string{"auth", "status", "--json"}) {
		t.Errorf("claude commands: %q, %q", claude.Args, claude.Status)
	}
	if claude.UserCode != nil || claude.CodePrompt == "" || claude.Success == "" {
		t.Errorf("claude takes the page's code at a prompt: %+v", claude)
	}
	url := "https://claude.com/cai/oauth/authorize?code=true&client_id=00000000-0000-0000-0000-000000000000&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&state=abc"
	if got := claude.URL.FindString("visit:  " + url + " \n"); got != url {
		t.Errorf("claude URL pattern found %q", got)
	}
	for out, want := range map[string]bool{
		"{\n  \"loggedIn\": true,\n  \"authMethod\": \"claude.ai\"\n}":                       true,
		"{\n  \"loggedIn\": false,\n  \"authMethod\": \"none\"\n}":                           false,
		"Not logged in. Run claude auth login to authenticate.":                              false,
		"{\"loggedIn\":true,\"configDirectory\":\"/state/home/.claude\",\"analytics\":true}": true,
	} {
		if got := claude.LoggedIn.MatchString(out); got != want {
			t.Errorf("claude status %q: logged in %v, want %v", out, got, want)
		}
	}

	codex, err := LoginFlowFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(codex.Args, []string{"login", "--device-auth"}) || !slices.Equal(codex.Status, []string{"login", "status"}) {
		t.Errorf("codex commands: %q, %q", codex.Args, codex.Status)
	}
	if codex.UserCode == nil || codex.CodePrompt != "" {
		t.Errorf("codex shows a one-time code and takes none: %+v", codex)
	}
	page := "1. Open this link in your browser and sign in to your account\n   https://auth.openai.com/codex/device\n\n" +
		"2. Enter this one-time code (expires in 15 minutes)\n   ABC1-DEF23\n"
	if got := codex.URL.FindString(page); got != "https://auth.openai.com/codex/device" {
		t.Errorf("codex URL pattern found %q", got)
	}
	if m := codex.UserCode.FindStringSubmatch(page); len(m) < 2 || m[1] != "ABC1-DEF23" {
		t.Errorf("codex code pattern found %q", m)
	}
	for out, want := range map[string]bool{
		"Logged in using ChatGPT":           true,
		"Logged in using an API key - sk-*": true,
		"Not logged in":                     false,
	} {
		if got := codex.LoggedIn.MatchString(out); got != want {
			t.Errorf("codex status %q: logged in %v, want %v", out, got, want)
		}
	}
}

func isRefusal(err error, stage string) bool {
	var re *RefusalError
	return errors.As(err, &re) && errors.Is(err, ErrRefused) && re.Stage == stage
}

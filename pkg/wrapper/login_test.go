package wrapper

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/contain"
)

// claudeLoginOutput is claude 2.1.270's `auth login` output as a terminal
// receives it, with the OAuth parameters' values changed: the URL as an OSC 8
// link and again as colored text, then the code prompt.
const claudeLoginOutput = "Opening browser to sign in\xe2\x80\xa6\r\n" +
	"If the browser didn't open, visit: \x1b]8;;" + claudeLoginURL + "\x07\x1b[94m" + claudeLoginURL + "\x1b[39m\x1b]8;;\x07\r\n" +
	"Paste code here if prompted > "

const claudeLoginURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=00000000-0000-0000-0000-000000000000" +
	"&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback" +
	"&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference&code_challenge=AAAA&code_challenge_method=S256&state=BBBB"

// codexLoginOutput is codex 0.144.5's `login --device-auth` output, with the
// one-time code changed.
const codexLoginOutput = "\r\nWelcome to Codex [v\x1b[90m0.144.5\x1b[0m]\r\n\x1b[90mOpenAI's command-line coding agent\x1b[0m\r\n\r\n" +
	"Follow these steps to sign in with ChatGPT using device code authorization:\r\n\r\n" +
	"1. Open this link in your browser and sign in to your account\r\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\r\n\r\n" +
	"2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\r\n   \x1b[94mWXY1-Z2345\x1b[0m\r\n\r\n" +
	"\x1b[90mContinue only if you started this login in Codex. If a website or another person gave you this code, cancel.\x1b[0m\r\n"

func TestTerminalText(t *testing.T) {
	for raw, want := range map[string]string{
		"a\x1b[1;31mb\x1b[0mc":                             "abc",
		"x\x1b]8;;https://e.test/p\x07link\x1b]8;;\x07":    "x https://e.test/p link",
		"x\x1b]8;id=1;https://e.test/p\x1b\\link":          "x https://e.test/p link",
		"x\x1b]0;window title\x07y":                        "xy",
		"one\r\ntwo\rthree\n":                              "one\ntwo\nthree\n",
		"{\r\r\n\x1b[3G\"loggedIn\":\x1b[15Gfalse,\r\r\n}": "{\n  \"loggedIn\": false,\n}",
		"a\x1b[2Cb\x1b[1Gc":                                "a  bc",
		"bell\x07 and \x1b(Bcharset":                       "bell and charset",
		"cut \x1b]8;;https://e.test/unfinished":            "cut ",
	} {
		if got := terminalText(raw); got != want {
			t.Errorf("terminalText(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestFindLoginPrompt(t *testing.T) {
	claude, err := contain.LoginFlowFor("claude")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := findLoginPrompt(claude, terminalText(claudeLoginOutput))
	if !ok || p.URL != claudeLoginURL || !p.WantsCode || p.UserCode != "" {
		t.Errorf("claude prompt = %+v, %v", p, ok)
	}
	// Without the code prompt the page is not yet all the person needs.
	if _, ok := findLoginPrompt(claude, terminalText(strings.TrimSuffix(claudeLoginOutput, "Paste code here if prompted > "))); ok {
		t.Error("claude prompt complete before the code prompt appeared")
	}

	codex, err := contain.LoginFlowFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	p, ok = findLoginPrompt(codex, terminalText(codexLoginOutput))
	if !ok || p.URL != "https://auth.openai.com/codex/device" || p.UserCode != "WXY1-Z2345" || p.WantsCode {
		t.Errorf("codex prompt = %+v, %v", p, ok)
	}
}

// TestLoginOutputAcrossWrites: however the output is split into writes, the
// prompt is found once, whole — never a URL or code cut short at the end of a
// write.
func TestLoginOutputAcrossWrites(t *testing.T) {
	for name, tc := range map[string]struct {
		harness, output string
		want            LoginPrompt
	}{
		"claude": {"claude", claudeLoginOutput, LoginPrompt{URL: claudeLoginURL, WantsCode: true}},
		"codex":  {"codex", codexLoginOutput, LoginPrompt{URL: "https://auth.openai.com/codex/device", UserCode: "WXY1-Z2345"}},
	} {
		flow, err := contain.LoginFlowFor(tc.harness)
		if err != nil {
			t.Fatal(err)
		}
		for _, size := range []int{1, 3, 7, 64} {
			o := newLoginOutput(flow, nil)
			for i := 0; i < len(tc.output); i += size {
				_, _ = o.Write([]byte(tc.output[i:min(i+size, len(tc.output))]))
				select {
				case <-o.ready:
					if got := o.prompt(); got != tc.want {
						t.Fatalf("%s, %d-byte writes: prompt %+v after %d bytes, want %+v", name, size, got, i+size, tc.want)
					}
				default:
				}
			}
			select {
			case <-o.ready:
			default:
				t.Errorf("%s, %d-byte writes: no prompt found", name, size)
			}
		}
	}
}

func TestLoginOutputSuccess(t *testing.T) {
	flow, err := contain.LoginFlowFor("claude")
	if err != nil {
		t.Fatal(err)
	}
	o := newLoginOutput(flow, nil)
	_, _ = o.Write([]byte(claudeLoginOutput + "code#state\r\nLogin succ"))
	select {
	case <-o.success:
		t.Fatal("success before its text was complete")
	default:
	}
	_, _ = o.Write([]byte("essful.\r\n"))
	select {
	case <-o.success:
	default:
		t.Fatal("no success after \"Login successful.\"")
	}
}

func TestLoginConfigValidation(t *testing.T) {
	for name, cfg := range map[string]LoginConfig{
		"no harness":  {BinaryPath: "/bin/true", StateDir: t.TempDir()},
		"no binary":   {Harness: "claude", StateDir: t.TempDir()},
		"no StateDir": {Harness: "claude", BinaryPath: "/bin/true"},
	} {
		if _, err := StartLogin(context.Background(), cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: got %v, want ErrInvalidConfig", name, err)
		}
	}
	cfg := LoginConfig{Harness: "claude", BinaryPath: "/bin/true", StateDir: t.TempDir()}
	if runtime.GOOS != "linux" {
		if _, err := StartLogin(context.Background(), cfg); !errors.Is(err, ErrContainmentUnsupported) {
			t.Errorf("got %v, want ErrContainmentUnsupported", err)
		}
		return
	}
	cfg.Containment = &Containment{Kind: ContainmentLandlock, StateDir: t.TempDir()}
	if _, err := StartLogin(context.Background(), cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("a Containment.StateDir that is not StateDir: got %v, want ErrInvalidConfig", err)
	}
	cfg.Containment, cfg.Harness = nil, "opencode"
	if _, err := StartLogin(context.Background(), cfg); !errors.Is(err, ErrContainmentRefused) {
		t.Errorf("a harness without a login flow: got %v, want ErrContainmentRefused", err)
	}
}

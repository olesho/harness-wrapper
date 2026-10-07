package adapter

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runHeaders writes each value to a file of its own and runs the headers
// script over them with /bin/sh, returning its stdout and stderr.
func runHeaders(t *testing.T, values map[string]string, prep func(files map[string]string)) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{}
	i := 0
	for k, v := range values {
		f := filepath.Join(dir, "h'"+string(rune('a'+i)))
		i++
		if err := os.WriteFile(f, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		files[k] = f
	}
	if prep != nil {
		prep(files)
	}
	script := filepath.Join(dir, "headers.sh")
	if err := os.WriteFile(script, []byte(HeadersScript("srv", files)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func TestHeadersScript(t *testing.T) {
	out, stderr, err := runHeaders(t, map[string]string{
		"Authorization": "Bearer tok\r\n",
		"X-Odd":         "a\"b\\c\td\n",
	}, nil)
	if err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %q: %v", out, err)
	}
	want := map[string]string{"Authorization": "Bearer tok", "X-Odd": "a\"b\\c\td"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("headers %v", got)
	}
}

func TestHeadersScriptRefusesControlCharacters(t *testing.T) {
	out, stderr, err := runHeaders(t, map[string]string{"X-K": "a\x01b"}, nil)
	if err == nil {
		t.Fatalf("a control character passed: %q", out)
	}
	if !strings.Contains(stderr, "control character") {
		t.Errorf("stderr %q", stderr)
	}
}

func TestHeadersScriptFailsOnAnUnreadableFile(t *testing.T) {
	out, stderr, err := runHeaders(t, map[string]string{"X-K": "v"}, func(files map[string]string) {
		if err := os.Remove(files["X-K"]); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatalf("a missing file passed: %q", out)
	}
	if out != "" || !strings.Contains(stderr, "cannot read the file of header X-K") {
		t.Errorf("stdout %q, stderr %q", out, stderr)
	}
}

// A file the test of -r passes but a read fails (a directory) fails the
// script: no header goes out empty.
func TestHeadersScriptFailsOnAFailedRead(t *testing.T) {
	out, stderr, err := runHeaders(t, map[string]string{"X-K": "v"}, func(files map[string]string) {
		if err := os.Remove(files["X-K"]); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(files["X-K"], 0o700); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatalf("a failed read passed: %q", out)
	}
	if out != "" || !strings.Contains(stderr, "cannot read the file of header X-K") {
		t.Errorf("stdout %q, stderr %q", out, stderr)
	}
}

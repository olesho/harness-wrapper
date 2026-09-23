package transcript

import "testing"

// TestParseLine: one record parses as ParseFromBytes parses each line —
// "role" standing in for a missing "type" — and a malformed record is an
// error, where ParseFromBytes skips it.
func TestParseLine(t *testing.T) {
	line, err := ParseLine([]byte(`{"role":"user","uuid":"u1","message":{"content":"hi"}}`))
	if err != nil || line.Type != TypeUser || line.UUID != "u1" {
		t.Fatalf("ParseLine = %+v, %v", line, err)
	}
	if _, err := ParseLine([]byte(`{"type":"user"`)); err == nil {
		t.Fatal("ParseLine accepted a truncated record")
	}
	if lines, _ := ParseFromBytes([]byte("{\"type\":\"user\"\n{\"type\":\"assistant\"}\n")); len(lines) != 1 || lines[0].Type != TypeAssistant {
		t.Fatalf("ParseFromBytes = %+v; want the malformed line skipped", lines)
	}
}

package transcript

import (
	"encoding/json"
	"testing"
)

func TestToCount(t *testing.T) {
	tests := []struct {
		name string
		in   json.Number
		want int
	}{
		{"positive", json.Number("42"), 42},
		{"negative", json.Number("-5"), 0},
		{"zero", json.Number("0"), 0},
		{"garbage", json.Number("not-a-number"), 0},
		{"empty", json.Number(""), 0},
		{"fractional truncates", json.Number("3.9"), 3},
		{"negative fractional clamps", json.Number("-3.9"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToCount(tt.in); got != tt.want {
				t.Errorf("ToCount(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestUsageSerializesAllFive guards the byte-identical parity requirement: all
// five keys must appear even when their values are zero (no inner omitempty).
func TestUsageSerializesAllFive(t *testing.T) {
	b, err := json.Marshal(Usage{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		"input_tokens",
		"output_tokens",
		"cache_read_input_tokens",
		"cache_creation_input_tokens",
		"reasoning_output_tokens",
	} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := m[key]; !ok {
			t.Errorf("key %q missing from zero-value Usage JSON %s", key, b)
		}
	}
}

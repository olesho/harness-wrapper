package sessionid

import "testing"

func TestNewUUID(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewUUID()
		if !IsUUID(id) {
			t.Fatalf("NewUUID() = %q, not a canonical UUID", id)
		}
		if id[14] != '4' {
			t.Fatalf("NewUUID() = %q, want version 4", id)
		}
		if v := id[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Fatalf("NewUUID() = %q, want the RFC 9562 variant", id)
		}
		if seen[id] {
			t.Fatalf("NewUUID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestIsUUID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"123e4567-e89b-12d3-a456-426614174000", true},
		{"123E4567-E89B-12D3-A456-426614174000", true},
		{"123e4567e89b12d3a456426614174000", false},
		{"123e4567-e89b-12d3-a456-42661417400", false},
		{"123e4567-e89b-12d3-a456-4266141740000", false},
		{"g23e4567-e89b-12d3-a456-426614174000", false},
		{" 123e4567-e89b-12d3-a456-426614174000", false},
		{"", false},
	} {
		if got := IsUUID(tc.in); got != tc.want {
			t.Errorf("IsUUID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

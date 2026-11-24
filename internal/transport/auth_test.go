package transport

import (
	"errors"
	"testing"
)

func TestNewToken_Unique(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" {
		t.Fatalf("empty token returned: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("two consecutive NewToken calls returned the same value: %q", a)
	}
	// 32 bytes -> 43 chars base64url no-padding.
	if len(a) < 40 || len(a) > 50 {
		t.Fatalf("unexpected token length: %d", len(a))
	}
}

// TestConstantTimeEqual covers the match, a mismatch of either length (the
// equal-length row exercises the constant-time branch), and an empty token
// on either side, which answers ErrEmptyToken rather than a mismatch.
func TestConstantTimeEqual(t *testing.T) {
	for _, tc := range []struct {
		name, server, supplied string
		wantErr, wantEmpty     bool
	}{
		{"match", "secret", "secret", false, false},
		{"mismatch", "secret", "wrong", true, false},
		{"equal-length mismatch", "aaaaaa", "bbbbbb", true, false},
		{"both empty", "", "", true, true},
		{"empty supplied", "server", "", true, true},
		{"empty server", "", "supplied", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ConstantTimeEqual(tc.server, tc.supplied)
			if (err != nil) != tc.wantErr || errors.Is(err, ErrEmptyToken) != tc.wantEmpty {
				t.Fatalf("ConstantTimeEqual(%q, %q) = %v, want error %v, ErrEmptyToken %v", tc.server, tc.supplied, err, tc.wantErr, tc.wantEmpty)
			}
		})
	}
}

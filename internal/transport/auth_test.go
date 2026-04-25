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

func TestConstantTimeEqual_Match(t *testing.T) {
	if err := ConstantTimeEqual("secret", "secret"); err != nil {
		t.Fatalf("matching tokens should not error: %v", err)
	}
}

func TestConstantTimeEqual_Mismatch(t *testing.T) {
	if err := ConstantTimeEqual("secret", "wrong"); err == nil {
		t.Fatalf("mismatched tokens should error")
	}
	// Same length to exercise the constant-time branch on equal-length
	// inputs (the timing-attack defense).
	if err := ConstantTimeEqual("aaaaaa", "bbbbbb"); err == nil {
		t.Fatalf("equal-length mismatched tokens should error")
	}
}

func TestConstantTimeEqual_Empty(t *testing.T) {
	if err := ConstantTimeEqual("", ""); !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("empty/empty should return ErrEmptyToken, got %v", err)
	}
	if err := ConstantTimeEqual("server", ""); !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("non-empty server / empty supplied should return ErrEmptyToken, got %v", err)
	}
	if err := ConstantTimeEqual("", "supplied"); !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("empty server / non-empty supplied should return ErrEmptyToken, got %v", err)
	}
}

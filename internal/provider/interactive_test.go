package provider

import (
	"errors"
	"testing"
)

func TestNormalizeUserInputDecision(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", "accept", false},
		{"accept", "accept", false},
		{"allow", "accept", false},
		{"deny", "decline", false},
		{"decline", "decline", false},
		{"cancel", "decline", false},
		{"bogus", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := NormalizeUserInputDecision(tt.input)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidUserInputDecision) {
					t.Fatalf("NormalizeUserInputDecision(%q) error = %v, want ErrInvalidUserInputDecision", tt.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeUserInputDecision(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeUserInputDecision(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

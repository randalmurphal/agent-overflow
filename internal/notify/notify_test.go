package notify

import (
	"reflect"
	"strings"
	"testing"
)

func TestTargetRoundTrip(t *testing.T) {
	want := Target{Kind: "thread", ThreadID: "thread-123"}
	data, err := TargetToMap(want)
	if err != nil {
		t.Fatalf("encode target: %v", err)
	}
	got, err := TargetFromMap(data)
	if err != nil {
		t.Fatalf("decode target: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestValidateTarget(t *testing.T) {
	tests := []struct {
		name   string
		target Target
	}{
		{name: "missing kind", target: Target{}},
		{name: "thread missing id", target: Target{Kind: "thread"}},
		{name: "none with id", target: Target{Kind: "none", ThreadID: "unexpected"}},
		{name: "oversized thread id", target: Target{Kind: "thread", ThreadID: strings.Repeat("x", MaxThreadIDBytes+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateTarget(tt.target); err == nil {
				t.Fatalf("validate %#v unexpectedly succeeded", tt.target)
			}
		})
	}
}

package main

import (
	"reflect"
	"testing"
)

func TestMutateDisabledSet(t *testing.T) {
	tests := []struct {
		name     string
		current  []string
		target   string
		disabled bool
		want     []string
	}{
		{"add to empty", nil, "x", true, []string{"x"}},
		{"add new", []string{"a"}, "b", true, []string{"a", "b"}},
		{"add idempotent", []string{"a", "b"}, "a", true, []string{"b", "a"}},
		{"remove present", []string{"a", "b"}, "a", false, []string{"b"}},
		{"remove absent", []string{"a"}, "z", false, []string{"a"}},
		{"remove from empty", nil, "x", false, []string{}},
		{"remove last", []string{"a"}, "a", false, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mutateDisabledSet(tt.current, tt.target, tt.disabled)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mutateDisabledSet(%v, %q, %v) = %v, want %v",
					tt.current, tt.target, tt.disabled, got, tt.want)
			}
		})
	}
}

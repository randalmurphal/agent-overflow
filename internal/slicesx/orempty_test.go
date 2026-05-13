package slicesx

import (
	"encoding/json"
	"testing"
)

func TestOrEmptyNilStrings(t *testing.T) {
	var in []string
	got := OrEmpty(in)
	if got == nil {
		t.Fatal("OrEmpty(nil) returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("OrEmpty(nil) length = %d, want 0", len(got))
	}
}

func TestOrEmptyPopulatedReturnsSameStorage(t *testing.T) {
	in := []string{"a", "b"}
	got := OrEmpty(in)
	if &got[0] != &in[0] {
		t.Fatal("OrEmpty(populated) re-allocated slice; expected shared storage")
	}
}

func TestOrEmptyEmptyNonNilReturnsSameStorage(t *testing.T) {
	in := []int{}
	got := OrEmpty(in)
	if got == nil {
		t.Fatal("OrEmpty([]int{}) returned nil")
	}
	if len(got) != 0 {
		t.Fatalf("OrEmpty([]int{}) length = %d, want 0", len(got))
	}
}

func TestOrEmptyMarshalsToEmptyArray(t *testing.T) {
	// The whole reason this helper exists: nil-coalesce so JSON
	// emits `[]` rather than `null`.
	var in []string
	got := OrEmpty(in)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("Marshal(OrEmpty(nil)) = %s, want []", encoded)
	}
}

func TestOrEmptyGenericOverStructs(t *testing.T) {
	type item struct{ N int }
	var in []item
	got := OrEmpty(in)
	if got == nil || len(got) != 0 {
		t.Fatalf("OrEmpty[struct](nil) = %v, want empty non-nil", got)
	}
}

package errorsx

import (
	"errors"
	"strings"
	"testing"
)

func TestAppendFiltersNil(t *testing.T) {
	if got := Append(nil, nil); got != nil {
		t.Fatalf("Append(nil, nil) = %v, want nil", got)
	}

	start := []error{errors.New("first")}
	got := Append(start, nil)
	if len(got) != 1 {
		t.Fatalf("len(Append) = %d, want 1", len(got))
	}

	err := errors.New("second")
	got = Append(got, err)
	if len(got) != 2 || got[1] != err {
		t.Fatalf("Append did not append the new error: %v", got)
	}
}

func TestWrapLifecyclePassThroughNil(t *testing.T) {
	if err := WrapLifecycle("close thing", nil); err != nil {
		t.Fatalf("WrapLifecycle(..., nil) = %v, want nil", err)
	}
}

func TestWrapLifecycleWrapsWithAction(t *testing.T) {
	base := errors.New("inner cause")
	wrapped := WrapLifecycle("shutdown db", base)
	if wrapped == nil {
		t.Fatal("WrapLifecycle returned nil for non-nil cause")
	}
	if !errors.Is(wrapped, base) {
		t.Fatalf("errors.Is(%v, base) = false, want true", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "shutdown db:") {
		t.Fatalf("error = %q, want action prefix", wrapped.Error())
	}
}

package store

import "testing"

// SwapRuntimeMode writes only over the mode the caller saw, so a writer that
// follows the provider never overwrites a mode the user picked meanwhile.
func TestSwapRuntimeModeWritesOnlyOverTheExpectedMode(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t1")
	if err := s.UpdateRuntimeMode("t1", "auto"); err != nil {
		t.Fatalf("UpdateRuntimeMode: %v", err)
	}

	changed, err := s.SwapRuntimeMode("t1", "full-access", "approval-required")
	if err != nil || changed {
		t.Fatalf("swap from a mode the row does not hold = (%v, %v), want (false, nil)", changed, err)
	}
	if got, _ := s.GetThread("t1"); got.RuntimeMode != "auto" {
		t.Fatalf("runtime mode = %q after a refused swap, want auto", got.RuntimeMode)
	}

	changed, err = s.SwapRuntimeMode("t1", "auto", "approval-required")
	if err != nil || !changed {
		t.Fatalf("swap from the held mode = (%v, %v), want (true, nil)", changed, err)
	}
	if got, _ := s.GetThread("t1"); got.RuntimeMode != "approval-required" {
		t.Fatalf("runtime mode = %q, want approval-required", got.RuntimeMode)
	}

	changed, err = s.SwapRuntimeMode("t1", "approval-required", "approval-required")
	if err != nil || changed {
		t.Fatalf("swap to the held mode = (%v, %v), want (false, nil)", changed, err)
	}

	if _, err := s.SwapRuntimeMode("missing", "auto", "approval-required"); err == nil {
		t.Fatal("swap on a missing thread = nil error, want one")
	}
}

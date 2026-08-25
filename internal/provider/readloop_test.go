package provider

import "testing"

func teardownContents(t *testing.T, closing bool) []string {
	t.Helper()
	var got []string
	EmitTeardownStatus(func(evt ProviderEvent) {
		if evt.Kind != EventSessionStatus {
			t.Errorf("teardown emitted kind %q, want %q", evt.Kind, EventSessionStatus)
		}
		if evt.ThreadID != "t1" {
			t.Errorf("teardown emitted threadID %q, want %q", evt.ThreadID, "t1")
		}
		if evt.Timestamp.IsZero() {
			t.Error("teardown event carries a zero timestamp")
		}
		got = append(got, evt.Content)
	}, "t1", nil, closing)
	return got
}

// TestEmitTeardownStatusOrder pins the pair triage depends on: an
// unrequested exit reports "error" FIRST (that is what gates the synthesized
// truncated turn-complete) and "disconnected" second. Reversing them, or
// dropping the error, leaves the frontend working indicator stuck on a thread
// whose process is already gone.
func TestEmitTeardownStatusOrder(t *testing.T) {
	got := teardownContents(t, false)
	want := []string{"error", "disconnected"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("abnormal teardown emitted %v, want %v", got, want)
	}
}

// TestEmitTeardownStatusHostInitiatedSkipsError pins the other direction: a
// close AO asked for is not a truncated turn, so the error must not fire —
// it would surface a session_died banner for an intentional teardown.
func TestEmitTeardownStatusHostInitiatedSkipsError(t *testing.T) {
	got := teardownContents(t, true)
	if len(got) != 1 || got[0] != "disconnected" {
		t.Fatalf("host-initiated teardown emitted %v, want [disconnected]", got)
	}
}

// TestEmitTeardownStatusNilCallback pins that a session torn down before it
// had an event sink does not panic on the way out.
func TestEmitTeardownStatusNilCallback(t *testing.T) {
	EmitTeardownStatus(nil, "t1", nil, false)
	EmitTeardownStatusWithMeta(nil, "t1", nil, false)
}

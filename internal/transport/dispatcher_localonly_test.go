package transport

import "testing"

type localOnlyReceiver struct{}

func (r *localOnlyReceiver) Ping() string { return "pong" }

// TestRegisterLocalOnlyReceiver covers the receiver-level LocalOnly
// flag the harness registration uses: every method on the receiver is
// refused for non-loopback peers with the same method_not_found shape
// an unregistered method produces, and stays callable from loopback.
func TestRegisterLocalOnlyReceiver(t *testing.T) {
	d := NewDispatcher()
	methods, err := d.Register(&localOnlyReceiver{}, RegisterOptions{
		Package:   "main",
		TypeName:  "Harness",
		LocalOnly: true,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(methods) != 1 || !methods[0].LocalOnly {
		t.Fatalf("methods = %+v, want one LocalOnly method", methods)
	}
	if methods[0].FQN != "main.Harness.Ping" {
		t.Fatalf("FQN = %q, want main.Harness.Ping", methods[0].FQN)
	}

	if _, fe := d.ResolveForOrigin(0, "Ping", true); fe != nil {
		t.Fatalf("loopback resolve refused: %+v", fe)
	}
	if _, fe := d.ResolveForOrigin(0, "Ping", false); fe == nil {
		t.Fatal("non-loopback peer resolved a LocalOnly-receiver method")
	} else if fe.Code != ErrCodeMethodNotFound {
		t.Fatalf("refusal code = %v, want method_not_found (unenumerable)", fe.Code)
	}
}

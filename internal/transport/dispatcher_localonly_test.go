package transport

import (
	"strings"
	"testing"
)

// localOnlyReceiver stands in for the one receiver class that is still
// judged by ORIGIN: a receiver registered RegisterOptions{LocalOnly}.
// The harness's is the only one in the tree, and it carries no
// `//ao:scope` annotations because methodgen does not generate for it —
// so there is no grant a session could hold that would answer for it
// either. Shared with server_test.go and dispatcher_test.go, which
// exercise the same receiver over a live connection and under
// contention.
//
// The per-METHOD origin partition that used to run beside this is gone
// (wave 6d2). Every off-host connection names a session, and what that
// session may call is AuthorizeSessionMethod's answer, pinned in
// authorize_test.go and reachability_test.go.
type localOnlyReceiver struct{}

func (r *localOnlyReceiver) Ping() string { return "pong" }

// openReceiver is registered on the same dispatcher without the option,
// so the refusal below is visibly about the receiver rather than about a
// non-loopback peer being broken.
type openReceiver struct{}

func (r *openReceiver) PublicEcho() string { return "ok" }

// TestRegisterLocalOnlyReceiver covers the receiver-level LocalOnly flag
// the harness registration uses: every method on the receiver is refused
// for non-loopback peers with the same method_not_found shape an
// unregistered method produces, and stays callable from loopback.
// Resolution by NAME and by the FNV id both take the refusal, since the
// wire may carry either.
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
	if _, err := d.Register(&openReceiver{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("Register open receiver: %v", err)
	}

	if _, fe := d.ResolveForOrigin(0, "Ping", true); fe != nil {
		t.Fatalf("loopback resolve refused: %+v", fe)
	}

	for _, probe := range []struct {
		name string
		id   uint32
		by   string
	}{
		{name: "Ping", by: "name"},
		{id: methods[0].ID, by: "id"},
	} {
		_, fe := d.ResolveForOrigin(probe.id, probe.name, false)
		if fe == nil {
			t.Fatalf("non-loopback peer resolved a LocalOnly-receiver method by %s", probe.by)
		}
		if fe.Code != ErrCodeMethodNotFound {
			t.Fatalf("refusal code by %s = %v, want method_not_found (unenumerable)", probe.by, fe.Code)
		}
		if !strings.Contains(fe.Message, "not registered") {
			t.Fatalf("refusal message by %s = %q, want the generic shape", probe.by, fe.Message)
		}
	}

	// Targeted: a method on an ordinary receiver still answers the same
	// peer. What narrows it from there is its scope, not its origin.
	if _, fe := d.ResolveForOrigin(0, "PublicEcho", false); fe != nil {
		t.Fatalf("non-loopback peer refused on an ordinary receiver: %+v", fe)
	}
}

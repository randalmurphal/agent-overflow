package transport

import (
	"context"
	"testing"

	"agent-overflow/internal/buildvariant"
)

// remoteNamedReceiver carries one method named like a generated //ao:remote
// row and one named like an ordinary row, so the dispatcher's refusal is
// judged by the generated classification, the same table every call
// path reads.
type remoteNamedReceiver struct{ calls int }

func (r *remoteNamedReceiver) AddBackend() string {
	r.calls++
	return "added"
}

func (r *remoteNamedReceiver) Version() string {
	r.calls++
	return "v"
}

// A build without remote access refuses every //ao:remote method before
// it runs, on the path every transport shares, and leaves ordinary methods
// alone. The standard build runs both.
func TestInvokeRefusesRemoteOnlyMethodsWithoutRemoteAccess(t *testing.T) {
	if !classify("AddBackend").Remote || classify("Version").Remote {
		t.Fatal("the generated table no longer marks AddBackend //ao:remote and Version ordinary; pick other fixture names")
	}
	receiver := &remoteNamedReceiver{}
	d := NewDispatcher()
	if _, err := d.Register(receiver, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	remote, fe := d.ResolveForOrigin(0, "AddBackend", true)
	if fe != nil {
		t.Fatalf("resolve AddBackend: %+v", fe)
	}
	ordinary, fe := d.ResolveForOrigin(0, "Version", true)
	if fe != nil {
		t.Fatalf("resolve Version: %+v", fe)
	}

	_, fe = d.Invoke(context.Background(), remote, nil)
	if buildvariant.RemoteAccess {
		if fe != nil {
			t.Fatalf("standard build refused AddBackend: %+v", fe)
		}
	} else {
		if fe == nil || fe.Code != ErrCodeMethodError || fe.Message != buildvariant.ErrRemoteAccessUnavailable.Error() {
			t.Fatalf("AddBackend answered %+v, want the remote-access refusal", fe)
		}
	}
	if _, fe := d.Invoke(context.Background(), ordinary, nil); fe != nil {
		t.Fatalf("Version refused: %+v", fe)
	}
	wantCalls := 2
	if !buildvariant.RemoteAccess {
		wantCalls = 1
	}
	if receiver.calls != wantCalls {
		t.Fatalf("receiver ran %d methods, want %d", receiver.calls, wantCalls)
	}
}

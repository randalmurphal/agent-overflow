package wsllauncher

import (
	"agent-overflow/internal/dispatchrefusal"
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/selfupdate"
	"agent-overflow/internal/transport"

	"github.com/coder/websocket"
)

// TestShutdownRPCNamesARegisteredHostMethod is the drift guard for the
// method name this package restates. A rename on the backend side that
// missed this copy would leave every window close falling back to the Job
// Object kill, which is exactly the failure the door exists to remove and
// which only a live Windows session would notice. The scope is asserted
// with it: an ordinary browser client must never be able to stop the
// backend, and `host` is what says so.
func TestShutdownRPCNamesARegisteredHostMethod(t *testing.T) {
	for _, method := range transport.GeneratedMethods {
		if method.Name != RPCShutdownBackend {
			continue
		}
		if method.Scope != transport.ScopeHost {
			t.Fatalf("%s is scoped %q, want %q", RPCShutdownBackend, method.Scope, transport.ScopeHost)
		}
		return
	}
	t.Fatalf("no bound method named %q; the launcher would fall back to the Job Object kill", RPCShutdownBackend)
}

// TestNotificationClientShutdownPostsTheBackendDoor pins what the launcher
// actually sends and that it waits for the backend's answer.
func TestNotificationClientShutdownPostsTheBackendDoor(t *testing.T) {
	called := make(chan notificationClientFrame, 1)
	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
		if err := expectSubscribeAndReplay(ctx, conn); err != nil {
			return err
		}
		frame, err := readClientFrame(ctx, conn)
		if err != nil {
			return err
		}
		called <- frame
		return writeServerFrame(ctx, conn, notificationServerFrame{Type: "rpc", ID: frame.ID})
	})

	client, _ := newTestBridgeClient(t, wsURL, func(selfupdate.InstallDirective) {})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go client.Run(ctx)

	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case frame := <-called:
		if frame.Type != "rpc" || frame.Method != RPCShutdownBackend {
			t.Fatalf("shutdown frame = %+v, want an rpc naming %s", frame, RPCShutdownBackend)
		}
		if len(frame.Params) != 0 {
			t.Fatalf("shutdown frame carries params %v; the door takes none", frame.Params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backend never saw the shutdown RPC")
	}
}

// TestNotificationClientShutdownSurfacesRefusal covers the fallback
// decision: a backend that ANSWERS and rejects is proof the graceful path
// did not run, so the caller must be able to tell that from a lost call
// and close the Job Object instead.
func TestNotificationClientShutdownSurfacesRefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal notificationFrameError
		want    string
	}{
		// A backend still sending the old text reads as the version sentence.
		{"a release before the dispatch mark", notificationFrameError{Code: "method_not_found", Message: "method not registered"},
			RPCShutdownBackend + " RPC method_not_found: " + dispatchrefusal.Unsupported},
		// A current backend says what did not fit beside it.
		{"this release", notificationFrameError{Code: "bad_params", Message: dispatchrefusal.Unreadable, Dispatch: dispatchrefusal.CountRefusal(RPCShutdownBackend, 0, 1, false, false)},
			RPCShutdownBackend + " RPC bad_params: " + dispatchrefusal.Unreadable + " (ShutdownBackend: expected 0 arguments, got 1)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal := tc.refusal
			wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
				if err := expectSubscribeAndReplay(ctx, conn); err != nil {
					return err
				}
				frame, err := readClientFrame(ctx, conn)
				if err != nil {
					return err
				}
				return writeServerFrame(ctx, conn, notificationServerFrame{Type: "rpc", ID: frame.ID, Error: &refusal})
			})

			client, _ := newTestBridgeClient(t, wsURL, func(selfupdate.InstallDirective) {})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go client.Run(ctx)

			err := client.Shutdown(ctx)
			var refused *RPCRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Shutdown error = %v, want an *RPCRefusedError", err)
			}
			if refused.Method != RPCShutdownBackend || refused.Code != refusal.Code || refused.Message != refusal.Message {
				t.Fatalf("refusal = %+v, want the shutdown door refused, its wire code and text kept", refused)
			}
			if err.Error() != tc.want {
				t.Fatalf("Error() = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

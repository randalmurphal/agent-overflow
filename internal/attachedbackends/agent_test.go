package attachedbackends

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

func TestAgentAccessIsExplicitAndRemovalClearsIt(t *testing.T) {
	m, dir := newManager(t)
	id := uuid.NewString()
	seed(t, dir, deviceclient.Session{BackendID: id, SessionID: "test-session", Credential: "test-credential", Endpoint: "https://127.0.0.1:1"})
	access, err := m.AgentAccess()
	if err != nil || access[id] {
		t.Fatalf("default: %v %v", access, err)
	}
	if err := m.CallAgentPeer(context.Background(), id, "RemoteCommandStart", nil); err == nil {
		t.Fatal("unenabled peer reached network")
	}
	if err := m.SetAgentAccess(id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(id); err != nil {
		t.Fatal(err)
	}
	seed(t, dir, deviceclient.Session{BackendID: id, SessionID: "test-session", Credential: "test-credential", Endpoint: "https://127.0.0.1:1"})
	access, err = m.AgentAccess()
	if err != nil || access[id] {
		t.Fatalf("re-pair revived opt-in: %v %v", access, err)
	}
	if err := m.SetAgentAccess(uuid.NewString(), true); err == nil {
		t.Fatal("enabled an unpaired computer")
	}
}

func TestAgentAccessBoundsAndCallSurface(t *testing.T) {
	m, dir := newManager(t)
	access := map[string]bool{}
	for range MaxAgentComputers + 1 {
		access[uuid.NewString()] = true
	}
	if err := atomicfile.WriteJSON(filepath.Join(dir, "agent-access.json"), access); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AgentAccess(); err == nil {
		t.Fatal("unbounded discovery configuration accepted")
	}
	if err := m.CallAgentPeer(context.Background(), uuid.NewString(), "DeleteProject", nil); err == nil || err.Error() != "this method is not available to agent commands" {
		t.Fatal("peer RPC surface widened beyond command operations:", err)
	}
}

// TestThreadPeerCallsAreTheirOwnSurfaceAndNeedNoOptIn pins the two rules
// that separate the thread tools reach from agent commands: its own
// method allowlist, and pairing alone as the reach.
func TestThreadPeerCallsAreTheirOwnSurfaceAndNeedNoOptIn(t *testing.T) {
	m, dir := newManager(t)
	id := uuid.NewString()
	seed(t, dir, deviceclient.Session{BackendID: id, SessionID: "test-session", Credential: "test-credential", Endpoint: "https://127.0.0.1:1"})

	err := m.CallThreadPeer(context.Background(), "not-a-computer", "ThreadToolCall", nil)
	if code, _, public := errorsx.PublicDetails(err); !public || code != "thread_unreachable" {
		t.Fatal("a computer id that is not a UUID reached the network:", err)
	}
	if err := m.CallThreadPeer(context.Background(), id, "RemoteCommandStart", nil); err == nil ||
		err.Error() != "this method is not available to agent thread tools" {
		t.Fatal("thread tools reached a method outside their surface:", err)
	}

	// The same computer, with no agent opt-in: a command is refused for
	// that reason, a thread call is not and goes on to dial.
	if code, _, public := errorsx.PublicDetails(m.CallAgentPeer(context.Background(), id, "RemoteCommandStart", nil)); !public || code != "remote_access_disabled" {
		t.Fatal("the agent command opt-in stopped applying")
	}
	err = m.CallThreadPeer(context.Background(), id, "ThreadToolCall", nil)
	if err == nil {
		t.Fatal("a call to an unreachable endpoint succeeded")
	}
	if code, _, public := errorsx.PublicDetails(err); public && code == "remote_access_disabled" {
		t.Fatal("thread tools were refused for want of the agent commands opt-in")
	}
}

// TestUnsupportedPeerErrorNamesTheSurfaceItAskedFor proves an older
// destination, which advertises no thread-tools capability, refuses with
// the thread tools code rather than the remote commands one.
func TestUnsupportedPeerErrorNamesTheSurfaceItAskedFor(t *testing.T) {
	err := unsupportedPeerError(transport.CapabilityThreadTools, errUnsupportedPeerOperation)
	code, message, public := errorsx.PublicDetails(err)
	if !public || code != "thread_unsupported" || !strings.Contains(message, "agent thread tools") {
		t.Fatalf("an older destination refused a thread call as %q %q", code, message)
	}
	if !errors.Is(err, errUnsupportedPeerOperation) {
		t.Fatal("the refusal dropped its cause")
	}
	if code, _, public := errorsx.PublicDetails(unsupportedPeerError(transport.CapabilityRemoteCommands, errUnsupportedPeerOperation)); !public || code != "remote_unsupported" {
		t.Fatalf("an older destination refused a command as %q", code)
	}
}

// A failed call says whether its request could have reached the
// destination: never when it failed before a proven connection had it,
// always once one did, whether the destination answered or the call was
// abandoned waiting.
func TestTheCarrierSaysWhetherAFailedCallWasSent(t *testing.T) {
	m, p := newThreadPeerManager(t)
	bounded := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 5*time.Second)
	}

	ctx, cancel := bounded()
	defer cancel()
	var remote *rpcclient.Error
	if err := m.CallThreadPeer(ctx, p.id, "ThreadToolResolve", nil); !errors.As(err, &remote) || NotSent(err) {
		t.Fatalf("an answered refusal = %v, NotSent %v; want it sent", err, NotSent(err))
	}
	waiting, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if err := m.CallThreadPeer(waiting, p.id, "ThreadToolCall", nil); !errors.Is(err, context.DeadlineExceeded) || NotSent(err) {
		t.Fatalf("a call abandoned waiting = %v, NotSent %v; want it sent", err, NotSent(err))
	}

	for name, call := range map[string]func(context.Context) error{
		"no such pairing": func(ctx context.Context) error {
			return m.CallThreadPeer(ctx, uuid.NewString(), "ThreadToolCall", nil)
		},
		"not a computer id": func(ctx context.Context) error {
			return m.CallThreadPeer(ctx, "not-a-computer", "ThreadToolCall", nil)
		},
		"a method outside the surface": func(ctx context.Context) error {
			return m.CallThreadPeer(ctx, p.id, "RemoteCommandStart", nil)
		},
		"cancelled before it was sent": func(ctx context.Context) error {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			return m.CallThreadPeer(cancelled, p.id, "ThreadToolCall", nil)
		},
		"a command not opted in": func(ctx context.Context) error {
			return m.CallAgentPeer(ctx, p.id, "RemoteCommandStart", nil)
		},
	} {
		ctx, cancel := bounded()
		if err := call(ctx); err == nil || !NotSent(err) {
			t.Errorf("%s = %v, want it marked not sent", name, err)
		}
		cancel()
	}

	// The computer goes away: its idle connection fails the ping before
	// reuse, and the dial that replaces it fails.
	p.closeAll()
	p.Close()
	ctx, cancel = bounded()
	defer cancel()
	if err := m.CallThreadPeer(ctx, p.id, "ThreadToolCall", nil); err == nil || !NotSent(err) {
		t.Fatalf("a call to a computer that cannot be reached = %v, want it marked not sent", err)
	}
}

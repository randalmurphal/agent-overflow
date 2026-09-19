package attachedbackends

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/errorsx"
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

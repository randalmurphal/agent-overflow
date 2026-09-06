package attachedbackends

import (
	"agent-overflow/internal/atomicfile"
	"context"
	"path/filepath"
	"testing"

	"agent-overflow/internal/deviceclient"
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

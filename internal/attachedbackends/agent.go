package attachedbackends

import (
	"context"
	"errors"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/transport"
)

const MaxAgentComputers = 16

// AgentAccess is opt-in at the originating computer. Pairing a screen with a
// computer never implicitly gives its agents access to that computer.
func (m *Manager) AgentAccess() (map[string]bool, error) {
	access := map[string]bool{}
	_, err := atomicfile.ReadJSON(filepath.Join(m.dir, "agent-access.json"), &access)
	if access == nil {
		access = map[string]bool{}
	}
	if err == nil {
		if len(access) > MaxAgentComputers {
			return nil, errors.New("too many enabled agent computers in the access configuration")
		}
		for id, enabled := range access {
			if !entityid.Valid(id) || !enabled {
				return nil, errors.New("invalid agent computer access configuration")
			}
		}
	}
	return access, err
}

func (m *Manager) SetAgentAccess(id string, enabled bool) error {
	if !entityid.Valid(id) {
		return errors.New("invalid computer identity")
	}
	unlock := m.profiles.Lock(id)
	defer unlock()
	if enabled {
		if _, err := deviceclient.LoadSession(m.dir, id); err != nil {
			return err
		}
	}
	return m.writeAgentAccess(id, enabled)
}

func (m *Manager) writeAgentAccess(id string, enabled bool) error {
	unlock := m.profiles.Lock("agent-access")
	defer unlock()
	access, err := m.AgentAccess()
	if err != nil {
		return err
	}
	if enabled {
		// A revoked credential can disappear without a Manager.Remove call.
		// Prune those opt-ins when writing, not on every command or UI read.
		for peer := range access {
			if _, err := deviceclient.LoadSession(m.dir, peer); errors.Is(err, deviceclient.ErrNoSession) {
				delete(access, peer)
			}
		}
		if !access[id] && len(access) >= MaxAgentComputers {
			return errors.New("at most 16 computers can be enabled for agent commands")
		}
		access[id] = true
	} else {
		if !access[id] {
			return nil
		}
		delete(access, id)
	}
	return atomicfile.WriteJSON(filepath.Join(m.dir, "agent-access.json"), access)
}

// CallAgentPeer opens one authenticated RPC exchange using the carrier's
// EXISTING rotating credential owner. It neither retries mutations nor copies
// a frontend credential. Each call rechecks opt-in, identity and capability.
func (m *Manager) CallAgentPeer(ctx context.Context, id, method string, result any, params ...any) error {
	switch method {
	case "RemoteCommandStart", "RemoteCommandStatus", "RemoteCommandCancel", "RemoteCommandProjects":
	default:
		return errors.New("this method is not available to agent commands")
	}
	access, err := m.AgentAccess()
	if err != nil {
		return err
	}
	if !access[id] && method != "RemoteCommandStatus" && method != "RemoteCommandCancel" {
		return errors.New("agent commands are not enabled for this computer")
	}
	return m.callAgentPeer(ctx, id, method, result, params...)
}

// CheckAgentPeer proves the pairing, protocol, and command scope before an
// owner opts in. It runs no command and does not enable access on its own.
func (m *Manager) CheckAgentPeer(ctx context.Context, id string) error {
	return m.callAgentPeer(ctx, id, "RemoteCommandProjects", nil)
}

func (m *Manager) callAgentPeer(ctx context.Context, id, method string, result any, params ...any) error {
	held, err := m.carrier(id)
	if err != nil {
		return errors.New("this computer is no longer paired")
	}
	rpc, err := held.openRPC(ctx, transport.CapabilityRemoteCommands)
	if err != nil {
		return err
	}
	defer rpc.Close()
	return rpc.Call(ctx, method, result, params...)
}

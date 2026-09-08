package attachedbackends

import (
	"context"
	"errors"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
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
	if !entityid.Valid(id) {
		return errorsx.Public("remote_invalid_computer", "computer_id must be a computer UUID from remote_computers.", nil)
	}
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
		return errorsx.Public("remote_access_disabled", "Agent commands are not enabled for this computer. Ask the user to enable the destination in Remote access → Agent access on the originating computer.", nil)
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
		if errors.Is(err, deviceclient.ErrNoSession) {
			return errorsx.Public("remote_not_paired", "This computer is no longer paired. Reconnect it in Remote access before retrying; keep existing request IDs.", err)
		}
		return errorsx.Public("remote_pairing_unavailable", "The originating computer could not load this pairing. Check its Remote access settings and local configuration file permissions before retrying.", err)
	}
	rpc, err := held.openRPC(ctx, transport.CapabilityRemoteCommands)
	if err != nil {
		switch {
		case errors.Is(err, errUnsupportedPeerOperation):
			return errorsx.Public("remote_unsupported", "This destination version does not support remote commands. Update Agent Overflow on that computer.", err)
		case errors.Is(err, deviceclient.ErrAwaitingConfirmation):
			return errorsx.Public("remote_pairing_pending", "Pairing is waiting for approval. Confirm the matching verification number on the destination computer.", err)
		case errors.Is(err, deviceclient.ErrSessionEnded), errors.Is(err, deviceclient.ErrNoSession):
			return errorsx.Public("remote_pairing_expired", "The destination no longer accepts this pairing. Reconnect it in Remote access; keep existing request IDs.", err)
		}
		return err
	}
	defer rpc.Close()
	return rpc.Call(ctx, method, result, params...)
}

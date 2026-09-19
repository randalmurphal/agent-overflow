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
	case "RemoteCommandStart", "RemoteCommandStatus", "RemoteCommandCancel", "RemoteCommandProjects", "RemoteCommandEnvironment", "RemoteCommandReadLog", "RemoteCommandSearchLog", "RemoteCommandArtifact", "RemoteCommandLogArtifact":
	default:
		return errors.New("this method is not available to agent commands")
	}
	access, err := m.AgentAccess()
	if err != nil {
		return err
	}
	if !access[id] && method != "RemoteCommandStatus" && method != "RemoteCommandCancel" && method != "RemoteCommandReadLog" && method != "RemoteCommandSearchLog" && method != "RemoteCommandArtifact" && method != "RemoteCommandLogArtifact" {
		return errorsx.Public("remote_access_disabled", "Agent commands are not enabled for this computer. Ask the user to enable the destination in Remote access → Agent remote tools on the originating computer.", nil)
	}
	return m.callPeer(ctx, id, transport.CapabilityRemoteCommands, method, result, params...)
}

// CheckAgentPeer proves the pairing, protocol, and command scope before an
// owner opts in. It runs no command and does not enable access on its own.
func (m *Manager) CheckAgentPeer(ctx context.Context, id string) error {
	return m.callPeer(ctx, id, transport.CapabilityRemoteCommands, "RemoteCommandProjects", nil)
}

// threadPeerMethods is the whole surface agent thread tools reach on a
// paired computer. It is separate from the command allowlist because the
// two carry different authority: a thread call never runs a shell.
var threadPeerMethods = map[string]struct{}{
	"ThreadToolResolve": {}, "ThreadToolQuery": {}, "ThreadToolCall": {},
	"ThreadToolRequestStatus": {}, "ThreadToolExportChunk": {},
}

// CallThreadPeer opens one authenticated RPC exchange for the agent thread
// tools, on the same rotating credential CallAgentPeer uses.
//
// There is no opt-in check: reach is pairing alone. Pairing a computer is
// the user saying these two computers are theirs, and the destination
// authorizes every call itself against the authenticated device
// (docs/specs/agent-thread-tools.md, "Reaching another computer"). The
// agent-commands opt-in exists because a remote command runs a shell;
// nothing here does.
func (m *Manager) CallThreadPeer(ctx context.Context, id, method string, result any, params ...any) error {
	if !entityid.Valid(id) {
		return errorsx.Public("thread_unreachable", "computer_id must be a computer UUID from thread_options.", nil)
	}
	if _, ok := threadPeerMethods[method]; !ok {
		return errors.New("this method is not available to agent thread tools")
	}
	return m.callPeer(ctx, id, transport.CapabilityThreadTools, method, result, params...)
}

// callPeer is the shared body: load the carrier, open an RPC that proves
// the destination advertises `capability`, and make the call. The
// capability is the only thing that differs between the two surfaces, and
// it is what turns an older destination into a clear refusal instead of a
// method error.
func (m *Manager) callPeer(ctx context.Context, id, capability, method string, result any, params ...any) error {
	held, err := m.carrier(id)
	if err != nil {
		if errors.Is(err, deviceclient.ErrNoSession) {
			return errorsx.Public("remote_not_paired", "This computer is no longer paired. Reconnect it in Remote access before retrying.", err)
		}
		return errorsx.Public("remote_pairing_unavailable", "The originating computer could not load this pairing. Check its Remote access settings and local configuration file permissions before retrying.", err)
	}
	rpc, err := held.openRPC(ctx, capability)
	if err != nil {
		switch {
		case errors.Is(err, errUnsupportedPeerOperation):
			return unsupportedPeerError(capability, err)
		case errors.Is(err, deviceclient.ErrAwaitingConfirmation):
			return errorsx.Public("remote_pairing_pending", "Pairing is waiting for approval. Confirm the matching verification number on the destination computer.", err)
		case errors.Is(err, deviceclient.ErrSessionEnded), errors.Is(err, deviceclient.ErrNoSession):
			return errorsx.Public("remote_pairing_expired", "The destination no longer accepts this pairing. Reconnect it in Remote access.", err)
		}
		return err
	}
	defer rpc.Close()
	return rpc.Call(ctx, method, result, params...)
}

// unsupportedPeerError names the surface an older destination does not
// advertise, so the caller reads a refusal about the feature it asked for
// rather than a method error.
func unsupportedPeerError(capability string, cause error) error {
	if capability == transport.CapabilityThreadTools {
		return errorsx.Public("thread_unsupported", "This destination version does not support agent thread tools. Update Agent Overflow on that computer.", cause)
	}
	return errorsx.Public("remote_unsupported", "This destination version does not support remote commands. Update Agent Overflow on that computer.", cause)
}

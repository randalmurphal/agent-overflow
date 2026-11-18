package attachedbackends

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
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

// CallAgentPeer makes one authenticated RPC call using the carrier's
// EXISTING rotating credential owner. It neither retries mutations nor copies
// a frontend credential. Each call rechecks opt-in, identity and capability.
func (m *Manager) CallAgentPeer(ctx context.Context, id, method string, result any, params ...any) error {
	if !entityid.Valid(id) {
		return rpcclient.NotSentError{Err: errorsx.Public("remote_invalid_computer", "computer_id must be a computer UUID from remote_computers.", nil)}
	}
	switch method {
	case "RemoteCommandStart", "RemoteCommandStatus", "RemoteCommandCancel", "RemoteCommandProjects", "RemoteCommandEnvironment", "RemoteCommandReadLog", "RemoteCommandSearchLog", "RemoteCommandArtifact", "RemoteCommandLogArtifact":
	default:
		return rpcclient.NotSentError{Err: errors.New("this method is not available to agent commands")}
	}
	access, err := m.AgentAccess()
	if err != nil {
		return rpcclient.NotSentError{Err: err}
	}
	if !access[id] && method != "RemoteCommandStatus" && method != "RemoteCommandCancel" && method != "RemoteCommandReadLog" && method != "RemoteCommandSearchLog" && method != "RemoteCommandArtifact" && method != "RemoteCommandLogArtifact" {
		return rpcclient.NotSentError{Err: errorsx.Public("remote_access_disabled", "Agent commands are not enabled for this computer. Ask the user to enable the destination in Remote access → Agent remote tools on the originating computer.", nil)}
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
	"ThreadToolRequestStatus": {}, "ThreadToolExportChunk": {}, "ThreadToolWatch": {},
}

// CallThreadPeer makes one authenticated RPC call for the agent thread
// tools, on the same rotating credential CallAgentPeer uses, to a
// destination that advertises transport.CapabilityThreadTools.
//
// There is no opt-in check: reach is pairing alone. Pairing a computer is
// the user saying these two computers are theirs, and the destination
// authorizes every call itself against the authenticated device
// (docs/specs/agent-thread-tools.md, "Reaching another computer"). The
// agent-commands opt-in exists because a remote command runs a shell;
// nothing here does.
func (m *Manager) CallThreadPeer(ctx context.Context, id, method string, result any, params ...any) error {
	return m.CallThreadPeerOn(ctx, id, transport.CapabilityThreadTools, method, result, params...)
}

// CallThreadPeerOn is CallThreadPeer for a destination that speaks the
// thread peer protocol generation `protocol` (transport.CapabilityThreadTools
// or a later one). A destination that does not advertise it is refused
// before anything is sent, which Unsupported reports.
func (m *Manager) CallThreadPeerOn(ctx context.Context, id, protocol, method string, result any, params ...any) error {
	_, err := m.CallThreadPeerProving(ctx, id, protocol, "", method, result, params...)
	return err
}

// CallThreadPeerProving is CallThreadPeerOn that also reports whether the
// connection the call went out on advertised generation, a later thread
// peer protocol generation. That is the destination's hello on it, read
// before the call was sent: a call that failed after it still reports it,
// and an answer that came is that same build's. It is false when no
// connection was proven.
func (m *Manager) CallThreadPeerProving(ctx context.Context, id, protocol, generation, method string, result any, params ...any) (bool, error) {
	return m.holdThreadPeer(ctx, id, protocol, generation, func(call PeerCall) error { return call(ctx, method, result, params...) })
}

// HoldThreadPeer runs use with calls that share one connection to the
// computer, as the chunks of one export copy do (holdPeer).
func (m *Manager) HoldThreadPeer(ctx context.Context, id string, use func(PeerCall) error) error {
	_, err := m.holdThreadPeer(ctx, id, transport.CapabilityThreadTools, "", use)
	return err
}

// holdThreadPeer is holdPeer for the thread tools' surface: a computer id,
// a thread peer protocol generation, and only the methods the tools reach.
func (m *Manager) holdThreadPeer(ctx context.Context, id, protocol, generation string, use func(PeerCall) error) (bool, error) {
	if !entityid.Valid(id) {
		return false, rpcclient.NotSentError{Err: errorsx.Public("thread_unreachable", "computer_id must be a computer UUID from thread_options.", nil)}
	}
	if protocol != transport.CapabilityThreadTools && protocol != transport.CapabilityThreadToolsV2 {
		return false, rpcclient.NotSentError{Err: fmt.Errorf("%s is not a thread peer protocol", protocol)}
	}
	return m.holdPeer(ctx, id, protocol, generation, func(call PeerCall) error {
		return use(func(ctx context.Context, method string, result any, params ...any) error {
			if _, ok := threadPeerMethods[method]; !ok {
				return rpcclient.NotSentError{Err: errors.New("this method is not available to agent thread tools")}
			}
			return call(ctx, method, result, params...)
		})
	})
}

// PeerCall makes one call on a connection holdPeer holds.
type PeerCall func(ctx context.Context, method string, result any, params ...any) error

// Unanswered reports whether a PeerCall ended without an answer: its
// connection closed or went quiet, or the call's own context ended. The hold
// has dropped that connection, so a read sent again goes over a fresh one,
// pinged or dialed, or is refused unsent once the pairing has ended. A
// refusal the computer answered and a refusal about the pairing are not
// unanswered.
func Unanswered(err error) bool {
	var answered *rpcclient.Error
	if err == nil || errors.As(err, &answered) {
		return false
	}
	_, _, refused := errorsx.PublicDetails(err)
	return !refused
}

// Unsupported reports whether a failed call was refused because the
// destination does not advertise the capability it needed.
func Unsupported(err error) bool { return errors.Is(err, errUnsupportedPeerOperation) }

func (m *Manager) callPeer(ctx context.Context, id, capability, method string, result any, params ...any) error {
	_, err := m.holdPeer(ctx, id, capability, "", func(call PeerCall) error { return call(ctx, method, result, params...) })
	return err
}

// holdPeer is the shared body: load the carrier, then hand use a call that
// takes or opens an RPC proving the destination advertises `capability`
// (peerconns.go) and keeps it for the calls after. Those go out without the
// pool's ping, because the connection has just answered. A call that ends
// without an answer closes the connection, which may hold a reply nobody
// reads, and the next call takes or opens another. A connection still open
// when use returns goes back to the pool. use makes one call at a time.
//
// The capability is the only thing that differs between the two surfaces,
// and it is what turns an older destination into a clear refusal instead of
// a method error. Every failure before a proven connection has the call is
// a rpcclient.NotSentError; from then on the RPC client says whether its
// frame was written. It reports whether the connection the last call went
// out on advertised generation ("" asks nothing).
func (m *Manager) holdPeer(ctx context.Context, id, capability, generation string, use func(PeerCall) error) (proved bool, err error) {
	held, err := m.carrier(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return false, rpcclient.NotSentError{Err: err}
		}
		return false, rpcclient.NotSentError{Err: pairingRefusal(err)}
	}
	var rpc *rpcclient.Client
	defer func() {
		if rpc != nil {
			held.peers.put(capability, rpc)
		}
	}()
	err = use(func(ctx context.Context, method string, result any, params ...any) error {
		// Once the carrier has left, a call is refused with its verdict
		// before it is sent. A call already in flight when it left fails on
		// its closed connection instead: the destination may have acted on
		// it, which a pairing refusal would deny.
		if verdict := held.peers.verdict(); verdict != nil {
			if rpc != nil {
				rpc.Close()
				rpc = nil
			}
			return rpcclient.NotSentError{Err: pairingRefusal(verdict)}
		}
		if rpc == nil {
			if rpc = held.peers.take(ctx, capability); rpc == nil {
				opened, err := openPeerRPC(ctx, held, capability)
				if err != nil {
					if verdict := held.peers.verdict(); verdict != nil {
						err = pairingRefusal(errors.Join(verdict, err))
					}
					return rpcclient.NotSentError{Err: err}
				}
				rpc = opened
			}
		}
		proved = generation != "" && rpc.Advertises(generation)
		err := rpc.Call(ctx, method, result, params...)
		var answered *rpcclient.Error
		if err != nil && !errors.As(err, &answered) {
			rpc.Close()
			rpc = nil
			return err
		}
		held.reached()
		return err
	})
	return proved, err
}

// pairingRefusal maps why a pairing cannot carry a call, its carrier's
// lookup failing or the verdict the carrier left with, onto the agent
// surfaces' codes. It is raised only before a call is sent, so its caller
// marks it not sent.
func pairingRefusal(err error) error {
	switch {
	case errors.Is(err, errPairingEnded):
		return errorsx.Public("remote_pairing_expired", "The destination no longer accepts this pairing. Reconnect it in Remote access.", err)
	case errors.Is(err, errPairingReplaced):
		return errorsx.Public("remote_pairing_replaced", "This computer was paired again while the call was on its way. Retry it.", err)
	case errors.Is(err, deviceclient.ErrNoSession):
		return errorsx.Public("remote_not_paired", "This computer is no longer paired. Reconnect it in Remote access before retrying.", err)
	}
	return errorsx.Public("remote_pairing_unavailable", "The originating computer could not load this pairing. Check its Remote access settings and local configuration file permissions before retrying.", err)
}

// openPeerRPC dials a new connection for callPeer and maps the refusals
// raised before any call is made onto the agent surfaces' codes.
func openPeerRPC(ctx context.Context, held *carrier, capability string) (*rpcclient.Client, error) {
	rpc, err := held.openRPC(ctx, capability)
	if err != nil {
		switch {
		case errors.Is(err, errUnsupportedPeerOperation):
			return nil, unsupportedPeerError(capability, err)
		case errors.Is(err, deviceclient.ErrAwaitingConfirmation):
			return nil, errorsx.Public("remote_pairing_pending", "Pairing is waiting for approval. Confirm the matching verification number on the destination computer.", err)
		case errors.Is(err, deviceclient.ErrSessionEnded), errors.Is(err, deviceclient.ErrNoSession):
			return nil, errorsx.Public("remote_pairing_expired", "The destination no longer accepts this pairing. Reconnect it in Remote access.", err)
		}
		return nil, err
	}
	return rpc, nil
}

// unsupportedPeerError names the surface an older destination does not
// advertise, so the caller reads a refusal about the feature it asked for
// rather than a method error.
func unsupportedPeerError(capability string, cause error) error {
	if strings.HasPrefix(capability, "thread-tools.") {
		return errorsx.Public("thread_unsupported", "This destination version does not support agent thread tools. Update Agent Overflow on that computer.", cause)
	}
	return errorsx.Public("remote_unsupported", "This destination version does not support remote commands. Update Agent Overflow on that computer.", cause)
}

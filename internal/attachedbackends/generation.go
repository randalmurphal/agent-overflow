package attachedbackends

import (
	"errors"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/errorsx"
)

// A pairing's generation names this installation's relationship with one
// computer, which outlives the session that carries it today. The request
// and command ledgers name the pairing their records were admitted under by
// it (Grant), and a call made for one of those records is refused by a
// carrier of any other generation before anything is sent
// (CallThreadPeerUnder, CallAgentPeerUnder).
//
// A generation is the id of the session that began the relationship. A
// session saved with none began its own: an explicit pairing, any pairing
// made while none was held, and a profile an older build saved, whose
// session id is the generation from its first read with nothing written. An
// automatic own-device upgrade of a pairing still held saves the generation
// it continues with the session it installs (AcceptOwnDeviceIntroduction).
// Retiring, revoking or forgetting a pairing deletes its profile, so
// whatever is paired next begins a generation of its own.

// generationOf is the pairing generation session carries.
func generationOf(session deviceclient.Session) string {
	if session.PairingGeneration != "" {
		return session.PairingGeneration
	}
	return session.SessionID
}

// Grant names the pairing held with one computer now by its generation.
// Empty when nothing is paired under that id; a profile that cannot be read
// is an error.
func (m *Manager) Grant(id string) (string, error) {
	session, err := deviceclient.LoadSession(m.dir, id)
	if errors.Is(err, deviceclient.ErrNoSession) || errors.Is(err, deviceclient.ErrInvalidBackendID) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return generationOf(session), nil
}

// ErrPairingNotHeld is a call made for a record of a pairing generation this
// installation no longer holds with that computer. Nothing was sent; the
// record's pairing has ended here.
var ErrPairingNotHeld = errors.New("attachedbackends: the pairing this call was made for is no longer held")

// peerCarrier resolves the carrier an agent call is made on, mapping a
// pairing that cannot be loaded onto the agent surfaces' codes.
func (m *Manager) peerCarrier(id string) (*carrier, error) {
	held, err := m.carrier(id)
	if errors.Is(err, deviceclient.ErrNoSession) {
		return nil, errorsx.Public("remote_not_paired", "This computer is no longer paired. Reconnect it in Remote access before retrying.", err)
	}
	if err != nil {
		return nil, pairingUnavailable(err)
	}
	return held, nil
}

// peerCarrierUnder is peerCarrier for a call made on behalf of a record of
// the pairing generation: a carrier of any other generation, or none,
// refuses it with ErrPairingNotHeld. The carrier is the call's own, so the
// pairing cannot change between this check and the call: a carrier its
// replacement retired sends nothing.
func (m *Manager) peerCarrierUnder(id, generation string) (*carrier, error) {
	held, err := m.carrier(id)
	switch {
	case errors.Is(err, deviceclient.ErrNoSession), err == nil && held.generation != generation:
		return nil, errorsx.Public("remote_not_paired",
			"This computer no longer holds the pairing this was made under. Reconnect the computer in Remote access if it is no longer paired.", ErrPairingNotHeld)
	case err != nil:
		return nil, pairingUnavailable(err)
	}
	return held, nil
}

func pairingUnavailable(err error) error {
	return errorsx.Public("remote_pairing_unavailable", "The originating computer could not load this pairing. Check its Remote access settings and local configuration file permissions before retrying.", err)
}

package attachedbackends

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
)

// OwnIdentity reads this installation's existing pairing key identity. Merely
// listing computers must never create a key or grant group membership.
func (m *Manager) OwnIdentity() (string, error) { return deviceclient.KeyThumbprint(m.dir) }

// EnrollOwnIdentity is called only when the owner explicitly joins devices.
func (m *Manager) EnrollOwnIdentity() (string, error) {
	if _, err := deviceclient.EnrollDeviceKey(m.dir); err != nil {
		return "", err
	}
	return m.OwnIdentity()
}

// CallOwnDevice is the membership protocol's authenticated direct hop. It
// reuses the carrier's credential owner and never follows a remote proxy or
// exposes arbitrary RPC access to callers of the introduction protocol.
func (m *Manager) CallOwnDevice(ctx context.Context, id, method string, result any, params ...any) error {
	switch method {
	case "ListOwnDevices", "RegisterOwnDevice", "SyncOwnDevices", "MintOwnDeviceIntroduction", "IntroduceOwnDevice", "AcceptOwnDeviceIntroduction":
	default:
		return errors.New("this method is not part of own-device enrollment")
	}
	held, err := m.carrier(ctx, id)
	if err != nil {
		return err
	}
	rpc, err := held.openRPC(ctx, owndevices.Capability)
	if err != nil {
		return err
	}
	defer rpc.Close()
	return timedPeerCall(ctx, rpc, method, result, params...)
}

// timedPeerCall makes one membership or device-name call and logs it as a
// peer_call.<method> step when it ran long. The method is a constant of
// this package, never a parameter. Agent and thread-tool calls are not
// timed here: they wait on work by design, so their duration is no stall.
func timedPeerCall(ctx context.Context, rpc *rpcclient.Client, method string, result any, params ...any) error {
	started := time.Now()
	err := rpc.Call(ctx, method, result, params...)
	logSlowStep(stepPeerCall+"."+method, started, err)
	return err
}

// Steps of reaching a paired computer over its socket, as logged.
const (
	slowStepComponent = "attachedbackends"
	stepPeerDial      = "peer_dial"
	stepPeerHello     = "peer_hello"
	stepPeerCall      = "peer_call"
)

// logSlowStep logs a step that outlasted the threshold (logging.LogSlowStep).
func logSlowStep(step string, started time.Time, err error) {
	logging.LogSlowStep(slowStepComponent, step, started, logging.ErrorOutcome(err))
}

func (c *carrier) openRPC(ctx context.Context, capability string) (*rpcclient.Client, error) {
	c.peers.mu.Lock()
	generation := c.peers.generation
	c.peers.mu.Unlock()
	ticket, err := c.client.Ticket(ctx)
	if err != nil {
		// The mint is where a revoked session is found out: its rotation
		// refuses, and that verdict retires the row the way a manifest's
		// does, so no loop keeps dialling a pairing that is gone.
		c.ended(err)
		return nil, err
	}
	address, err := c.client.DialURL(ticket)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Transport: c.client.RoundTripper(), CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("computer redirects are refused")
	}}
	started := time.Now()
	conn, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: httpClient})
	dialOutcome := logging.ErrorOutcome(err)
	if err != nil && response != nil {
		dialOutcome = logging.StatusOutcome(response.StatusCode)
	}
	logging.LogSlowStep(slowStepComponent, stepPeerDial, started, dialOutcome)
	if err != nil {
		// net/http includes the one-use socket ticket in URL.Error.URL.
		// Retain the useful network cause without publishing that credential.
		var requestError *url.Error
		if errors.As(err, &requestError) {
			return nil, fmt.Errorf("could not connect to the paired computer: %w", requestError.Err)
		}
		if response != nil {
			return nil, fmt.Errorf("the paired computer refused the connection (HTTP %d)", response.StatusCode)
		}
		return nil, errors.New("could not connect to the paired computer")
	}
	rpc := rpcclient.New(conn)
	started = time.Now()
	hello, err := rpc.Hello(ctx)
	logSlowStep(stepPeerHello, started, err)
	if err == nil && (hello.BackendID != c.client.Session().BackendID || hello.ProtocolVersion != transport.ProtocolVersion) {
		err = errors.New("paired computer identity or protocol changed")
	}
	if err == nil && !slices.Contains(hello.Capabilities, capability) {
		err = errUnsupportedPeerOperation
	}
	if err == nil {
		err = c.client.ObserveComputerRoutes(ctx, hello.BackendID, hello.Routes)
	}
	if err != nil {
		rpc.Close()
		return nil, err
	}
	// From here the connection belongs to this pairing: it closes when the
	// carrier leaves, whoever is using it.
	if err := c.peers.admit(rpc, generation); err != nil {
		return nil, err
	}
	c.reached()
	return rpc, nil
}

// AcceptOwnDeviceIntroduction installs a direct session only if it is missing.
// The caller must first resolve the destination and its routes from active
// membership. The invitation cannot introduce a different address or pin.
// The destination independently binds redemption to our key.
//
// It reports added once the pairing is saved and published, even when the
// rotation that follows fails; the error then says why. ctx bounds route
// selection and the wait for that rotation. The redemption between them is
// an enrollment (enroll), which Forget and the start of shutdown cancel and
// ctx does not.
func (m *Manager) AcceptOwnDeviceIntroduction(ctx context.Context, raw string, routes []computerroute.Route) (bool, error) {
	link, err := deviceclient.DecodeLink(raw)
	if err != nil {
		return false, err
	}
	if link.Purpose != "own-introduction" {
		return false, errors.New("this invitation does not authorize automatic own-device enrollment")
	}
	initial, err := computerroute.Normalize(computerroute.Route{Endpoint: link.Endpoint, CertFingerprint: link.CertFingerprint})
	if err != nil || !slices.Contains(routes, initial) {
		return false, errors.New("introduction does not match an active own computer's address and certificate")
	}
	if m.selfID != nil && link.BackendID == m.selfID() {
		return false, errors.New("cannot connect a computer to itself")
	}
	// Checked here so a refused introduction probes nothing, and again by
	// the enrollment before its redemption and under the profile lock.
	if wanted, err := m.introductionWanted(link.BackendID); err != nil || !wanted {
		return false, err
	}
	probe, stop := m.untilShutdown(ctx)
	route, err := deviceclient.SelectPairingRoute(probe, link.BackendID, routes, deviceclient.WithDialContext(m.dial))
	stop()
	if err != nil {
		return false, err
	}
	link.Endpoint, link.CertFingerprint = route.Endpoint, route.CertFingerprint
	_, held, err := m.enroll(link, introductionByPeer, false)
	if errors.Is(err, errNotWanted) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if held == nil {
		return false, ErrClosed
	}
	// Desktop clients watch the profile set, not the public membership
	// catalog. Publish this edge before reconciliation sees it as present.
	m.notifyMembershipChanged()
	defer m.WakeOwnDevices()
	// The introduction is confirmed before its redemption is answered, so no
	// person is waited for. This one rotation is what tells the other
	// computer the introduction completed: until its redemption's refresh
	// secret is spent, that computer's next introduction of this device
	// revokes the session (store.RetireOwnIntroductions). ctx bounds only this
	// caller's wait; Forget and the manager's life end the rotation itself.
	if err := held.client.Renew(ctx); err != nil {
		return true, fmt.Errorf("attachedbackends: acknowledge the introduction to %s: %w", link.BackendID, err)
	}
	return true, nil
}

// introductionWanted reports whether an introduction to id may enroll: the
// person has not forgotten it here, and no own-device pairing with it exists.
func (m *Manager) introductionWanted(id string) (bool, error) {
	if excluded, err := m.ownDeviceExcluded(id); err != nil || excluded {
		return false, err
	}
	if session, err := deviceclient.LoadSession(m.dir, id); err == nil && session.OwnDevice {
		return false, nil
	} else if err != nil && !errors.Is(err, deviceclient.ErrNoSession) {
		return false, err
	}
	return true, nil
}

func (m *Manager) ConnectedOwnDeviceIDs() ([]string, error) {
	sessions, err := deviceclient.ListSessions(m.dir)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		if session.OwnDevice {
			ids = append(ids, session.BackendID)
		}
	}
	return ids, nil
}

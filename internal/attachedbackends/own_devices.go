package attachedbackends

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
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
	held, err := m.carrier(id)
	if err != nil {
		return err
	}
	rpc, err := held.openRPC(ctx, owndevices.Capability)
	if err != nil {
		return err
	}
	defer rpc.Close()
	return rpc.Call(ctx, method, result, params...)
}

func (c *carrier) openRPC(ctx context.Context, capability string) (*rpcclient.Client, error) {
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
	conn, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: httpClient})
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
	hello, err := rpc.Hello(ctx)
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
	c.reached()
	return rpc, nil
}

// AcceptOwnDeviceIntroduction installs a direct session only if it is missing.
// The caller must first resolve the destination and its routes from active
// membership. The invitation cannot introduce a different address or pin.
// The destination independently binds redemption to our key.
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
	unlock, err := m.profiles.LockCtx(ctx, link.BackendID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if excluded, err := m.ownDeviceExcluded(link.BackendID); err != nil || excluded {
		return false, err
	}
	if session, err := deviceclient.LoadSession(m.dir, link.BackendID); err == nil && session.OwnDevice {
		return false, nil
	} else if err != nil && !errors.Is(err, deviceclient.ErrNoSession) {
		return false, err
	}
	route, err := deviceclient.SelectPairingRoute(ctx, link.BackendID, routes, deviceclient.WithDialContext(m.dial))
	if err != nil {
		return false, err
	}
	link.Endpoint, link.CertFingerprint = route.Endpoint, route.CertFingerprint
	if _, err := m.addLinkLocked(ctx, link); err != nil {
		return false, err
	}
	// Introductions are already approved and must not create a human
	// confirmation wait. Their activation still verifies the issued session.
	m.mu.Lock()
	held := m.carriers[link.BackendID]
	m.mu.Unlock()
	if err := held.client.AwaitActivation(ctx); err != nil {
		return false, err
	}
	// Desktop clients watch the profile set, not the public membership
	// catalog. Publish this edge before reconciliation sees it as present.
	m.notifyMembershipChanged()
	m.WakeOwnDevices()
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

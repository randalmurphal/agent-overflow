// Package attachedbackends is the desktop's set of OTHER machines: the
// backends this installation has paired with and now carries on its own
// listener, so one page can drive several computers without ever holding
// more than one credential (docs/specs/remote-access.md §10).
//
// It is the join between three packages that each know one third of the
// job and none of the others:
//
//   - internal/deviceclient owns the pairing, the device key, the pinned
//     certificate and the rotating session. One file per backend under a
//     profile directory this package is handed.
//   - internal/backendproxy owns the hop: bytes in, bytes out, the
//     session's credential swapped in on the way out.
//   - internal/transport owns the routes and who may reach them.
//
// What this package adds is the set: which profiles exist, what to call
// them, and one live carrier per profile. It holds no history, no merged
// view and no reachability probe of the machines it holds — the SPA merges
// what several backends say, and each socket is the only current answer to
// whether the machine behind it is awake. Discovery probes candidates a
// person is about to pair with, and a carried manifest fetch is the page's
// own request; that fetch is also the one place the far side's verdict on
// a session is read and acted on.
package attachedbackends

import (
	"agent-overflow/internal/pairbootstrap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sync/singleflight"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/backendproxy"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/transport"
)

// Manager is one installation's attached backends.
//
// The set itself is NOT cached: every listing reads the profile directory,
// which is what makes an attach, a rename and a removal take effect with
// no invalidation to get wrong. What is cached is the live carrier per
// profile, because a carrier owns a rotating session and building a second
// one for the same backend would mean two processes rotating one refresh
// secret against each other.
type Manager struct {
	own       ownDeviceReconciler
	dir       string
	dial      deviceclient.DialContextFunc
	selfID    func() string
	discovery singleflight.Group

	// changed is the one observer of set mutations (SetChanged).
	changed func(SetChange)
	// labelGetter reads this installation's current name; label is the static
	// fallback for embedders. Platform describes this process, not a pairing.
	labelGetter func() (string, error)
	label       string
	platform    string

	mu       sync.Mutex
	carriers map[string]*carrier
	profiles *keyedlock.Registry
}

// New builds a manager over one device profile directory. The directory
// need not exist yet — it is created by the first pairing.
func New(dir, label, platform string) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("attachedbackends: a device profile directory is required")
	}
	return &Manager{dir: dir, label: label, platform: platform, carriers: map[string]*carrier{}, profiles: keyedlock.New()}, nil
}

// Attached implements transport.AttachedBackends.
//
// A profile whose file cannot be read is skipped by ListSessions rather
// than failing the listing, so one damaged profile never makes the other
// machines unreachable.
func (m *Manager) Attached() []transport.AttachedProfile {
	sessions, err := deviceclient.ListSessions(m.dir)
	if err != nil || len(sessions) == 0 {
		return nil
	}
	profiles := make([]transport.AttachedProfile, 0, len(sessions))
	for _, session := range sessions {
		profiles = append(profiles, transport.AttachedProfile{
			ID:        session.BackendID,
			BackendID: session.BackendID,
			Name:      displayName(session),
			Nickname:  session.Nickname,
		})
	}
	return profiles
}

// Carrier implements transport.AttachedBackends. Nil is an ordinary
// answer: a page whose manifest is a moment stale asks for a backend that
// has just been removed.
func (m *Manager) Carrier(id string) transport.BackendCarrier {
	held, err := m.carrier(id)
	if err != nil {
		return nil
	}
	// A typed nil returned through an interface is not nil, so the miss
	// has to be reported as an untyped one here.
	return held
}

// carrier resolves the live hop for one profile, building it on first use.
func (m *Manager) carrier(id string) (*carrier, error) {
	unlock := m.profiles.Lock(id)
	defer unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.carriers[id]; ok {
		if !held.client.Retired() {
			return held, nil
		}
		// A retired owner never authorizes again: the far side ended its
		// session, or another process re-paired over its profile. Either
		// way the file on disk is the current answer, so it is read again
		// rather than cached as a client that answers 503 until restart.
		delete(m.carriers, id)
	}
	session, err := deviceclient.LoadSession(m.dir, id)
	if err != nil {
		return nil, err
	}
	client, err := deviceclient.Open(m.dir, session, deviceclient.WithDialContext(m.dial))
	if err != nil {
		return nil, err
	}
	built, err := newCarrier(client, id)
	if err != nil {
		return nil, err
	}
	m.wire(built, id)
	m.carriers[id] = built
	return built, nil
}

// wire attaches this manager's hooks to a freshly built carrier and starts
// its session renewal, which reports through those hooks.
func (m *Manager) wire(built *carrier, id string) {
	built.labelGetter, built.platform = m.localLabel, m.platform
	built.nameSyncChanged = func() { m.notifyChanged(SetChange{Action: SetDeviceNameSync, ID: id}) }
	built.onEnded = func() { m.endSession(id, built) }
	built.keepRenewed(id)
}

// endSession is the verdict path: the far side stopped honouring one
// pairing for good. deviceclient has already dropped the refused session
// file and retired the owner; what is left is this process's cache and the
// people watching it. The carrier leaves so nothing keeps answering for a
// session that is gone, its agent opt-in goes the way a removal takes it,
// and the observer retires the row. A carrier a newer pairing has already
// replaced is not the cached one, and nothing here touches it.
func (m *Manager) endSession(id string, held *carrier) {
	m.mu.Lock()
	current := m.carriers[id] == held
	if current {
		delete(m.carriers, id)
	}
	m.mu.Unlock()
	if !current {
		return
	}
	held.stopRenewal()
	_ = m.writeAgentAccess(id, false)
	m.notifyChanged(SetChange{Action: SetRemoved, ID: id, Reason: RemovedByComputer})
}

// Attached is one attached machine as the desktop's own admin surface
// sees it. Richer than the manifest row, which a page needs only to open
// sockets with.
type Attached struct {
	// ID is this installation's name for the profile, and the id in every
	// carried route. Today it is the backend id — deviceclient files one
	// session per backend id — but the two are kept apart because one is
	// an address on this listener and the other is a machine's identity.
	ID string `json:"id"`
	// BackendID is what that machine called itself when this device
	// paired with it, and what a client keys its replica by.
	BackendID string `json:"backendId"`
	// Name is what to show: the owner's nickname, else the machine's own
	// name, else its address.
	Name string `json:"name"`
	// Nickname is what the owner typed, empty when they have not.
	Nickname string `json:"nickname,omitempty"`
	// Endpoint is the address this device pinned, in the spelling the
	// pairing link used.
	Endpoint string `json:"endpoint"`
	// LastReachedMs is when this machine last answered this process, Unix
	// milliseconds, zero for "not since this launch".
	//
	// Last-known and nothing more. Nothing here probes: waking every
	// attached laptop to answer one page load would make a boot as slow
	// as the slowest of them, and a page's own socket is the only current
	// answer anyway.
	LastReachedMs       int64  `json:"lastReachedMs,omitempty"`
	DeviceNameSyncError string `json:"deviceNameSyncError,omitempty"`
	OwnDeviceSyncError  string `json:"ownDeviceSyncError,omitempty"`
}

// List reads every attached machine.
func (m *Manager) List() ([]Attached, error) {
	sessions, err := deviceclient.ListSessions(m.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Attached, 0, len(sessions))
	for _, session := range sessions {
		row := Attached{
			ID:        session.BackendID,
			BackendID: session.BackendID,
			Name:      displayName(session),
			Nickname:  session.Nickname,
			Endpoint:  session.Endpoint,
		}
		m.mu.Lock()
		if held, ok := m.carriers[session.BackendID]; ok {
			row.LastReachedMs = held.lastReachedMs.Load()
			row.DeviceNameSyncError = held.nameError()
			row.OwnDeviceSyncError = held.ownError()
		}
		m.mu.Unlock()
		out = append(out, row)
	}
	return out, nil
}

// Attachment is what an attach attempt answers immediately: the pairing
// is real and the credential is stored, and it admits NOTHING until the
// owner of that machine matches VerificationNumber on their own screen.
type Attachment struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Endpoint           string `json:"endpoint"`
	VerificationNumber string `json:"verificationNumber"`
}

// Add spends a pairing link and returns as soon as the far side has
// issued a credential — which is BEFORE that credential admits anything.
//
// It deliberately does not wait for the confirmation. Waiting is
// `deviceclient.Client.AwaitActivation`, whose window is ten minutes,
// and an RPC that blocked for it would exceed every timeout on the wire
// between here and the page. So the two halves are split: this call
// answers the number a person has to compare, and Await runs the wait on
// its own and reports the outcome as an event.
func (m *Manager) Add(ctx context.Context, pairingLink string) (Attachment, error) {
	link, err := deviceclient.DecodeLink(pairingLink)
	comparison := ""
	addressPairing := err != nil && !strings.Contains(pairingLink, "#")
	if addressPairing {
		label, nameErr := m.localLabel()
		if nameErr != nil {
			return Attachment{}, nameErr
		}
		hc := pairbootstrap.NewHTTPClient(m.dial)
		defer hc.CloseIdleConnections()
		// Public metadata may refuse redundant setup, never authorize it.
		// Check before occupying the other computer's pairing window, and
		// check again under the profile lock after the sealed exchange.
		if info, inspectErr := inspectComputer(ctx, hc, pairingLink); inspectErr == nil {
			if m.selfID != nil && info.BackendID == m.selfID() {
				return Attachment{}, errors.New("this is the computer you are already using")
			}
			if _, savedErr := deviceclient.LoadSession(m.dir, info.BackendID); savedErr == nil {
				return Attachment{}, errors.New("this computer is already connected; use its existing connection")
			}
		}
		invite, number, exchangeErr := pairbootstrap.Exchange(ctx, hc, pairingLink, label, m.platform)
		if exchangeErr != nil {
			return Attachment{}, exchangeErr
		}
		link, err = deviceclient.DecodeLink(invite.URL)
		comparison = number
	}
	if err != nil {
		return Attachment{}, err
	}
	if m.selfID != nil && link.BackendID == m.selfID() {
		return Attachment{}, errors.New("this is the computer you are already using")
	}
	unlock, err := m.profiles.LockCtx(ctx, link.BackendID)
	if err != nil {
		return Attachment{}, err
	}
	defer unlock()
	if addressPairing {
		if _, savedErr := deviceclient.LoadSession(m.dir, link.BackendID); savedErr == nil {
			return Attachment{}, errors.New("this computer is already connected; use its existing connection")
		}
	}
	// An explicit pairing is a new grant. Do not revive an old agent opt-in
	// after revocation or an incomplete pairing; the explicit enable follows
	// it. This clearing belongs to THIS path only — own-device enrollment
	// shares addLinkLocked and must not clobber a live opt-in when it upgrades
	// an already-paired computer to a group session.
	if err := m.writeAgentAccess(link.BackendID, false); err != nil {
		return Attachment{}, err
	}
	attachment, err := m.addLinkLocked(ctx, link)
	if err != nil {
		return Attachment{}, err
	}
	if err := m.excludeOwnDevice(link.BackendID, false); err != nil {
		return Attachment{}, err
	}
	if comparison != "" {
		attachment.VerificationNumber = comparison
	}
	return attachment, nil
}

// Caller holds the destination profile lock across one enrollment. It installs
// the pairing session and nothing more — the agent-command opt-in is a separate
// grant this primitive never touches, so own-device re-enrollment preserves it.
func (m *Manager) addLinkLocked(ctx context.Context, link deviceclient.Link) (Attachment, error) {
	m.mu.Lock()
	if old := m.carriers[link.BackendID]; old != nil {
		old.stopRenewal()
		old.client.Retire()
	}
	delete(m.carriers, link.BackendID)
	m.mu.Unlock()
	label, err := m.localLabel()
	if err != nil {
		return Attachment{}, err
	}
	client, pairing, err := deviceclient.Pair(ctx, m.dir, link, label, m.platform, deviceclient.WithDialContext(m.dial))
	if err != nil {
		return Attachment{}, err
	}
	built, err := newCarrier(client, link.BackendID)
	if err != nil {
		return Attachment{}, err
	}
	m.mu.Lock()
	// A re-pairing with a machine already attached replaces the carrier,
	// because the session behind the old one was just superseded.
	m.wire(built, link.BackendID)
	m.carriers[link.BackendID] = built
	m.mu.Unlock()
	return Attachment{
		ID:                 link.BackendID,
		Name:               displayName(client.Session()),
		Endpoint:           link.Endpoint,
		VerificationNumber: pairing.VerificationNumber,
	}, nil
}

// Await blocks until the owner of the far machine confirms the number,
// refuses it, or lets the window close. Its caller is a goroutine, not an
// RPC.
func (m *Manager) Await(ctx context.Context, id string) error {
	held, err := m.carrier(id)
	if err != nil {
		return err
	}
	if err := held.client.AwaitActivation(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	current := m.carriers[id] == held
	m.mu.Unlock()
	if !current {
		return deviceclient.ErrSessionEnded
	}
	held.reached()
	return nil
}

// Remove forgets one machine. The device key survives: it names this
// DEVICE, and the far side adopts its row by thumbprint if this
// installation ever pairs with it again.
func (m *Manager) Remove(id string) error {
	unlock := m.profiles.Lock(id)
	defer unlock()
	if err := m.excludeOwnDevice(id, true); err != nil {
		return err
	}
	if err := m.forgetLocked(id); err != nil {
		return err
	}
	m.notifyChanged(SetChange{Action: SetRemoved, ID: id})
	return nil
}

// forgetLocked drops one profile; the caller holds its profile lock. The
// live owner, when there is one, deletes the pairing it still owns under
// the session transaction fence and retires, so a renewal already in flight
// cannot write the credential back. It leaves the cache before that call
// rather than during it: m.mu is not held across the OS lock, and the
// caller's profile lock is what keeps a second owner from being built from
// the file in the meantime. A removal that failed retires the old owner
// all the same, so the retry that rebuilds a carrier never has two.
func (m *Manager) forgetLocked(id string) error {
	if err := m.writeAgentAccess(id, false); err != nil {
		return err
	}
	m.mu.Lock()
	held := m.carriers[id]
	delete(m.carriers, id)
	m.mu.Unlock()
	if held == nil {
		return deviceclient.ForgetSession(m.dir, id)
	}
	held.stopRenewal()
	if err := held.client.Forget(); err != nil {
		held.client.Retire()
		return err
	}
	return nil
}

// Rename sets the owner's own label for one machine, or clears it when
// the nickname is empty.
//
// Written through the live client rather than straight to the file,
// because a rotation is replacing that same file whenever the credential
// comes due — and a rename that raced one would either lose the nickname
// or roll the credential back.
func (m *Manager) Rename(id, nickname string) error {
	held, err := m.carrier(id)
	if err != nil {
		return err
	}
	if err := held.client.SetNickname(nickname); err != nil {
		return err
	}
	m.notifyChanged(SetChange{Action: SetRenamed, ID: id, Nickname: nickname})
	return nil
}

// RepairAddress adds a verified replacement address to an
// existing pairing. The live client owns the profile write and route change.
func (m *Manager) RepairAddress(ctx context.Context, id, endpoint string) (string, error) {
	held, err := m.carrier(id)
	if err != nil {
		return "", err
	}
	route, err := held.client.RepairAddress(ctx, endpoint)
	return route.Endpoint, err
}

// carrier is one attached machine's live hop.
type carrier struct {
	labelGetter     func() (string, error)
	platform        string
	nameSync        atomic.Bool
	nameSyncChanged func()
	// onEnded is the manager's verdict path (endSession), fired once per
	// carrier by ended below.
	onEnded       func()
	nameErrorMu   sync.Mutex
	nameSyncError string
	ownSyncError  string
	client        *deviceclient.Client
	proxy         *backendproxy.Carrier
	// stopRenewal ends keepRenewed when this carrier leaves the manager.
	stopRenewal context.CancelFunc

	// lastReachedMs is when this machine last answered, Unix
	// milliseconds. One atomic, written where an answer arrives and read
	// by the admin listing. Not a read model: nothing is derived from it
	// and nothing waits on it.
	lastReachedMs atomic.Int64
}

func newCarrier(client *deviceclient.Client, name string) (*carrier, error) {
	wsURL, err := client.WebSocketURL()
	if err != nil {
		return nil, err
	}
	proxy, err := backendproxy.New(backendproxy.Config{
		WSURL:  wsURL,
		Paired: client,
		// The page reaches this machine's attachment bytes under a
		// per-backend subtree of this listener. That prefix is local
		// addressing and is stripped before the request crosses.
		TransferPrefix: transport.AttachedTransferPrefix + name,
		Name:           name,
	})
	if err != nil {
		return nil, err
	}
	return &carrier{client: client, proxy: proxy, stopRenewal: func() {}}, nil
}

// keepRenewed rotates the session before each access window closes for
// as long as this carrier is held. The page's sockets ride one session for
// hours, and the far side judges every call on that session's current
// window; a rotation only at dial time left them refused for age between
// the window closing and the next dial. A verdict from the far side takes
// the same path a refused manifest does.
func (c *carrier) keepRenewed(id string) {
	ctx, cancel := context.WithCancel(context.Background())
	c.stopRenewal = cancel
	go func() {
		err := c.client.KeepRenewed(ctx, func(err error) {
			log.Printf("attachedbackends: renew the session with %s: %v", id, err)
		})
		if ctx.Err() != nil || c.ended(err) {
			return
		}
		log.Printf("attachedbackends: session renewal with %s stopped: %v", id, err)
	}()
}

func (c *carrier) reached() { c.lastReachedMs.Store(time.Now().UnixMilli()) }

// ended reports a typed verdict that this pairing is finished and hands it
// to the manager. deviceclient retires the owner on every such verdict; an
// ErrSessionEnded from a session that merely cannot rotate leaves the owner
// live, and stays the transient answer it always was.
func (c *carrier) ended(err error) bool {
	if !errors.Is(err, deviceclient.ErrSessionEnded) && !errors.Is(err, deviceclient.ErrNoSession) || !c.client.Retired() {
		return false
	}
	if c.onEnded != nil {
		c.onEnded()
	}
	return true
}

// credentialRefused is the set the SPA and the --connect stub treat as a
// verdict on a credential rather than an outage
// (frontend/src/lib/transport/bootstrap.ts CREDENTIAL_REFUSED_STATUSES).
func credentialRefused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound
}

// Manifest asks the far machine what it says about itself.
//
// The far side's answer is decoded here and narrowed to the closed list
// transport.AttachedManifest declares, so a field that machine adds later
// cannot start answering for this page by arriving in a JSON body.
func (c *carrier) Manifest(ctx context.Context) (transport.AttachedManifest, error) {
	status, body, err := c.proxy.FetchBootstrap(ctx)
	if err == nil && credentialRefused(status) {
		// The browser's rule (bootstrap.ts): one renewal, one retry. A
		// refused manifest alone is ambiguous — an aged credential and a
		// revoked session answer the same 404 — and the rotation is what
		// tells them apart: a live session rotates and the retry serves,
		// a dead one refuses the rotation, which is the verdict.
		if err = c.client.Renew(ctx); err == nil {
			status, body, err = c.proxy.FetchBootstrap(ctx)
		}
	}
	if err != nil {
		if c.ended(err) {
			return transport.AttachedManifest{}, fmt.Errorf("%w: %w", transport.ErrAttachedSessionEnded, err)
		}
		return transport.AttachedManifest{}, err
	}
	if status != http.StatusOK {
		// Every other non-200 is one answer here: not reachable right
		// now. A pairing still awaiting confirmation lands here too — its
		// rotation is read-only and answers no verdict yet.
		return transport.AttachedManifest{}, fmt.Errorf(
			"attachedbackends: %s answered its manifest with %d", c.proxy.BootstrapURL(), status)
	}
	var manifest struct {
		BackendID         string `json:"backendId"`
		ReplicaGeneration string `json:"replicaGeneration"`
		BackendName       string `json:"backendName"`
		LaunchID          string `json:"launchId"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return transport.AttachedManifest{}, fmt.Errorf("attachedbackends: decode the manifest: %w", err)
	}
	c.reached()
	return transport.AttachedManifest{
		SessionScopes:     c.client.Session().Scopes,
		BackendID:         manifest.BackendID,
		ReplicaGeneration: manifest.ReplicaGeneration,
		BackendName:       manifest.BackendName,
		LaunchID:          manifest.LaunchID,
	}, nil
}

func (c *carrier) CarryUpgrade(w http.ResponseWriter, r *http.Request) {
	c.syncDeviceName()
	c.reached()
	c.proxy.CarryUpgrade(w, r)
}

func (c *carrier) CarryTransfer(w http.ResponseWriter, r *http.Request) {
	c.proxy.CarryTransfer(w, r)
}

// displayName is what to call a machine: what its owner typed, else what
// it called itself when this device paired, else where it is. Never
// empty, because a row with no label is a row nobody can act on.
func displayName(session deviceclient.Session) string {
	if session.Nickname != "" {
		return session.Nickname
	}
	if session.BackendName != "" {
		return session.BackendName
	}
	return session.Endpoint
}

// SetLabelGetter wires the installation identity before serving requests.
func (m *Manager) SetLabelGetter(get func() (string, error)) { m.labelGetter = get }
func (m *Manager) localLabel() (string, error) {
	if m.labelGetter != nil {
		return m.labelGetter()
	}
	return m.label, nil
}

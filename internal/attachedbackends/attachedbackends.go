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
// them, and one live carrier per profile. It holds no history, no
// reachability probe and no merged view — the SPA merges what several
// backends say, and each socket is the only current answer to whether the
// machine behind it is awake.
package attachedbackends

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	dir string

	// label and platform are what this installation asks to be called in
	// the far side's device list. Fixed for the process: they describe
	// this machine, not one pairing.
	label    string
	platform string

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
		return held, nil
	}
	session, err := deviceclient.LoadSession(m.dir, id)
	if err != nil {
		return nil, err
	}
	client, err := deviceclient.Open(m.dir, session)
	if err != nil {
		return nil, err
	}
	built, err := newCarrier(client, id)
	if err != nil {
		return nil, err
	}
	m.carriers[id] = built
	return built, nil
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
	LastReachedMs int64 `json:"lastReachedMs,omitempty"`
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
	if err != nil {
		return Attachment{}, err
	}
	unlock, err := m.profiles.LockCtx(ctx, link.BackendID)
	if err != nil {
		return Attachment{}, err
	}
	defer unlock()
	// Pairing is a new grant. Do not revive an old agent opt-in after
	// revocation or an incomplete pairing; the explicit enable follows it.
	if err := m.writeAgentAccess(link.BackendID, false); err != nil {
		return Attachment{}, err
	}
	m.mu.Lock()
	if old := m.carriers[link.BackendID]; old != nil {
		old.client.Retire()
	}
	delete(m.carriers, link.BackendID)
	m.mu.Unlock()
	client, pairing, err := deviceclient.Pair(ctx, m.dir, link, m.label, m.platform)
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
	if err := m.writeAgentAccess(id, false); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if held := m.carriers[id]; held != nil {
		if err := held.client.Forget(); err != nil {
			return err
		}
	}
	delete(m.carriers, id)
	return deviceclient.ForgetSession(m.dir, id)
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
	return held.client.SetNickname(nickname)
}

// RepairAddress adds an explicitly entered, verified alternative to an
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
	client *deviceclient.Client
	proxy  *backendproxy.Carrier

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
	return &carrier{client: client, proxy: proxy}, nil
}

func (c *carrier) reached() { c.lastReachedMs.Store(time.Now().UnixMilli()) }

// Manifest asks the far machine what it says about itself.
//
// The far side's answer is decoded here and narrowed to the closed list
// transport.AttachedManifest declares, so a field that machine adds later
// cannot start answering for this page by arriving in a JSON body.
func (c *carrier) Manifest(ctx context.Context) (transport.AttachedManifest, error) {
	status, body, err := c.proxy.FetchBootstrap(ctx)
	if err != nil {
		return transport.AttachedManifest{}, err
	}
	if status != http.StatusOK {
		// Every non-200 is one answer here: not reachable right now.
		// Which of them means "this device was removed" is a question
		// deviceclient answers on its own schedule, by rotating and
		// forgetting a session the far side has refused.
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

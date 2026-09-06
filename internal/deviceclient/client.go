package deviceclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Client is this installation's attachment to one paired backend: the
// device key, the rotating session, and the pinned transport all three of
// them are presented over.
//
// One per backend, and safe for concurrent use — a `--connect` stub asks
// it for a ticket on every carried upgrade while a status loop asks it for
// headers, and the rotation rules below are only rules if both callers
// observe one exchange.
type Client struct {
	// dir is the profile directory: where the key lives, and where the
	// session file this client rewrites on every rotation lives.
	dir string
	key *ecdsa.PrivateKey
	// base is the URL this client dials, which is not always the endpoint
	// the payload printed (dialBase).
	base   string
	http   *http.Client
	routes *routeTransport
	// now is the clock, so a test can age a credential without sleeping.
	// Never nil: New fills it.
	now func() time.Time

	mu sync.Mutex
	// session is the stored pair, held in memory because it is read on
	// every request and written only on rotation.
	session Session
	// Retired clients cannot write a replacement profile or authorize a
	// request. This fences late renewals after removal or re-pairing.
	retired bool
	// renewing is the in-flight rotation, or nil. Two callers asking at
	// once must observe ONE exchange: a second concurrent renewal would
	// present a secret the first already spent, which the backend reads
	// as reuse evidence and answers by ending the session.
	renewing *renewal
}

// renewal is one rotation, shared by every caller that arrived while it
// was in flight. The error is read only after done closes, which is the
// happens-before that lets a waiter read it without the lock.
type renewal struct {
	done chan struct{}
	err  error
}

// renewMargin is how close to expiry a credential is rotated before it is
// used rather than after it is refused.
//
// One minute: long enough that a ticket minted with the old credential
// cannot lapse between the mint and the upgrade it is for, and small
// against the shortest access window the backend issues. The browser half
// uses the same number for the same reasons
// (frontend/src/lib/transport/deviceSession.ts).
const renewMargin = time.Minute

// ErrSessionEnded means the backend will not honour this session again:
// revoked, expired past renewal, or a refresh secret that was presented
// twice. The stored session is gone by the time a caller sees this, and
// the only way back is a fresh pairing link.
//
// One error for every such refusal, deliberately. The reason codes differ
// and the audit log keeps them, but the person's next action does not
// change between them, and a client that branched would offer four
// spellings of "pair this device again".
var ErrSessionEnded = errors.New(
	"deviceclient: this backend no longer honours this device's session; pair this device again")

// ErrAwaitingConfirmation means the session is real and admits nothing
// yet: the owner has not matched the verification number. The credential
// is kept — this is the one refusal where presenting the same credential
// again is expected to succeed shortly.
var ErrAwaitingConfirmation = errors.New(
	"deviceclient: this pairing is waiting for the owner to confirm the verification number")

// Refusal is a credential route's typed refusal: the HTTP status and the
// code from `internal/identity`'s closed set. Carried rather than
// flattened so a caller that wants the code for a log has it, while
// callers that only need to act use the sentinels above.
type Refusal struct {
	Status int
	Reason string
}

func (r *Refusal) Error() string {
	if r.Reason == "" {
		return fmt.Sprintf("deviceclient: the backend refused this credential (HTTP %d)", r.Status)
	}
	return fmt.Sprintf("deviceclient: the backend refused this credential (%s)", r.Reason)
}

// grant is the wire shape both credential-issuing routes answer with
// (transport.TokenGrant), plus the refusal body's one field. One struct,
// because a refusal and a grant arrive on the same route and reading them
// apart would mean deciding which to decode before knowing which it is.
type grant struct {
	refreshRecovery      bool
	SessionID            string   `json:"sessionId"`
	Credential           string   `json:"credential"`
	ExpiresAtMs          int64    `json:"expiresAtMs"`
	RefreshSecret        string   `json:"refreshSecret"`
	RefreshExpiresAtMs   int64    `json:"refreshExpiresAtMs"`
	AwaitingConfirmation bool     `json:"awaitingConfirmation"`
	VerificationNumber   string   `json:"verificationNumber"`
	PairingID            string   `json:"pairingId"`
	Scopes               []string `json:"scopes"`
	Reason               string   `json:"reason"`
}

// Pairing is what a completed redemption tells the person running it.
type Pairing struct {
	// BackendName and Endpoint are what the link named, echoed back so a
	// terminal can say which machine it is pairing with before the number.
	BackendName string
	Endpoint    string
	// VerificationNumber is the six digits the owner compares against
	// their own screen. It comes from the redemption RESPONSE — the
	// backend derives it from the key this device just proved it holds
	// (`internal/identity.Sessions.VerificationNumber`), which is what
	// makes a mismatch mean "some other device redeemed this link"
	// rather than "somebody mistyped".
	VerificationNumber string
	// SessionID and Scopes are what was issued. The session admits
	// nothing until the owner confirms.
	SessionID string
	Scopes    []string
}

// Pair spends a pairing link and enrolls this installation as a device.
//
// The order is the mechanism, and it is `internal/identity`'s rather than
// this package's: the key is generated and persisted FIRST, the redemption
// presents a signed proof over its own request, and the thumbprint the
// backend records is therefore derived from a key this process has
// demonstrated it holds. There is no separate "register the key" step that
// could be skipped, and a link read off a screen buys nothing without it.
//
// The returned credential is real and admits nothing yet. Every
// presentation refuses until the owner matches Pairing.VerificationNumber,
// which is what AwaitActivation waits for.
func Pair(ctx context.Context, dir string, link Link, label, platform string) (*Client, Pairing, error) {
	key, err := EnrollDeviceKey(dir)
	if err != nil {
		return nil, Pairing{}, err
	}
	base, err := dialBase(link.Endpoint, link.CertFingerprint)
	if err != nil {
		return nil, Pairing{}, err
	}
	client := &Client{
		dir:  dir,
		key:  key,
		base: base,
		http: credentialHTTPClient(link.CertFingerprint),
		now:  time.Now,
	}

	body, err := json.Marshal(struct {
		Token string `json:"token"`
		// KeyThumbprint is deliberately absent. It is the carrier for a
		// device that cannot sign, and the backend ignores it entirely
		// whenever a header proof is present — sending one anyway would
		// be a second, weaker claim about the key this proof already
		// names.
		Label    string `json:"label,omitempty"`
		Platform string `json:"platform,omitempty"`
	}{Token: link.Token, Label: label, Platform: platform})
	if err != nil {
		return nil, Pairing{}, fmt.Errorf("deviceclient: encode the redemption: %w", err)
	}

	issued, err := client.exchange(ctx, authPairPath, body, nil)
	if err != nil {
		return nil, Pairing{}, err
	}

	session := Session{
		RefreshRecovery:    &issued.refreshRecovery,
		BackendID:          link.BackendID,
		BackendName:        link.BackendName,
		Endpoint:           link.Endpoint,
		CertFingerprint:    link.CertFingerprint,
		SessionID:          issued.SessionID,
		Credential:         issued.Credential,
		ExpiresAtMs:        issued.ExpiresAtMs,
		RefreshSecret:      issued.RefreshSecret,
		RefreshExpiresAtMs: issued.RefreshExpiresAtMs,
		Scopes:             issued.Scopes,
		Label:              label,
	}
	if err := SaveSession(dir, session); err != nil {
		return nil, Pairing{}, err
	}
	client.session = session
	client.installRoutes()
	return client, Pairing{
		BackendName:        link.BackendName,
		Endpoint:           link.Endpoint,
		VerificationNumber: issued.VerificationNumber,
		SessionID:          issued.SessionID,
		Scopes:             issued.Scopes,
	}, nil
}

// Open builds a client over a session this installation already holds.
//
// It reads the device key and never mints one: a profile whose session
// survived while its key did not is a device that can no longer sign for
// the thumbprint its session names, and a fresh key would be refused one
// round trip later under a reason describing a different problem. The
// caller gets ErrNoDeviceKey and can forget the session.
func Open(dir string, session Session) (*Client, error) {
	key, err := DeviceKey(dir)
	if err != nil {
		return nil, err
	}
	base, err := dialBase(session.Endpoint, session.CertFingerprint)
	if err != nil {
		return nil, err
	}
	client := &Client{
		dir:     dir,
		key:     key,
		base:    base,
		http:    credentialHTTPClient(session.CertFingerprint),
		now:     time.Now,
		session: session,
	}
	client.installRoutes()
	return client, nil
}

// Session returns a snapshot of the stored session. A copy: the caller
// reads fields off it while a rotation may be replacing this client's own.
func (c *Client) Session() Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retired {
		return Session{BackendID: c.session.BackendID, Endpoint: c.session.Endpoint}
	}
	held := c.session
	held.Scopes = append([]string(nil), held.Scopes...)
	held.Routes = slices.Clone(held.Routes)
	if held.RefreshRecovery != nil {
		supported := *held.RefreshRecovery
		held.RefreshRecovery = &supported
	}
	return held
}

// Endpoint is the stable base callers address, promoted to HTTPS when pinned.
// The shared RoundTripper may select a verified alternative before sending.
func (c *Client) Endpoint() string { return c.base }

// RoundTripper is the pinned transport, for a caller that has to make its
// own request to this backend — the `--connect` stub's reverse proxy, and
// the manifest probe beside it. Handing out the transport rather than a
// second configuration is what keeps "every request to this backend is
// verified the same way" a property of the client instead of a convention.
func (c *Client) RoundTripper() http.RoundTripper { return c.http.Transport }

// Authorize attaches this device's session credential and a proof minted
// for THIS request.
//
// The proof binds the method and the path, so it must be minted against
// the request that will carry it and never reused: a cached one is refused
// as `proof_replayed`, and one carried to another route as
// `proof_not_bound`.
func (c *Client) Authorize(req *http.Request) error {
	c.mu.Lock()
	if c.retired {
		c.mu.Unlock()
		return ErrNoSession
	}
	credential := c.session.Credential
	c.mu.Unlock()
	if credential == "" {
		return ErrNoSession
	}
	proof, err := mintProof(c.key, req.Method, req.URL.Path, c.now())
	if err != nil {
		return err
	}
	req.Header.Set(SessionCredentialHeader, credential)
	req.Header.Set(DeviceKeyHeader, proof)
	return nil
}

// BootstrapURL is the backend's manifest, which a paired device reaches
// with its own credential rather than with a launch token.
func (c *Client) BootstrapURL() string { return c.base + bootstrapPath }

// Ticket mints the single-use ticket the WebSocket upgrade names this
// session with, and is the one call that both rotates and retries.
//
// The shape is the browser's (`deviceSession.mintDialTicket`) because the
// backend's rules are the same on both sides:
//
//   - A credential inside renewMargin of expiry is rotated BEFORE the
//     mint, so the ticket cannot outlive the credential that bought it.
//   - A refused mint is ambiguous — `/auth/ticket` answers the
//     unfingerprintable 404 for "not admitted yet" and "not admitted any
//     more" alike — so ONE rotation tells them apart: a live session
//     rotates and the retried mint succeeds, a dead one refuses the
//     rotation, which forgets the stored session and answers
//     ErrSessionEnded.
//   - The retry mints a FRESH proof. The first attempt's is spent.
func (c *Client) Ticket(ctx context.Context) (string, error) {
	if err := c.renewIfStale(ctx); err != nil {
		return "", err
	}
	ticket, err := c.mintTicket(ctx)
	if err == nil {
		return ticket, nil
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		return "", err
	}
	if err := c.renew(ctx); err != nil {
		return "", err
	}
	return c.mintTicket(ctx)
}

// DialURL is the WebSocket endpoint a freshly minted ticket names its
// session on. Separate from Ticket because the caller decides when to
// spend it: the ticket is single-use and lives seconds, so it is minted
// immediately before the dial and never held.
func (c *Client) DialURL(ticket string) (string, error) {
	return webSocketURL(c.base, ticket)
}

// WebSocketURL is the same endpoint with no ticket on it, for a caller
// that mints its own per upgrade — which is what a long-lived reverse
// proxy has to do, since a ticket baked into its target would be spent by
// the first handshake and refused by every one after it.
func (c *Client) WebSocketURL() (string, error) {
	return webSocketURL(c.base, "")
}

// probeInterval and probeDeadline are how the terminal waits for the owner
// to confirm. They match the browser pairing screen's constants exactly
// (frontend/src/lib/components/pairing/PairingScreen.svelte), because they
// are answers about the BACKEND — the confirmation window is ten minutes
// (identity.PairingConfirmWindow) and the credential routes share one
// per-peer budget — and two clients waiting on one window should not
// disagree about how long it is.
const (
	probeInterval = 3 * time.Second
	probeDeadline = 10 * time.Minute
)

// AwaitActivation blocks until the owner confirms the verification number,
// the pairing is refused, or the confirmation window closes.
//
// The probe is a ticket mint, which is the cheapest authenticated call
// that distinguishes admitted from not — and NOT a rotation, because a
// rotation that succeeded would spend the refresh secret once per poll for
// no reason. The ticket it mints goes unused and lapses in seconds, which
// the ticket book prices in.
//
// The window closing is not the same answer as the pairing being refused,
// and the difference matters to the person waiting: the owner may have
// cancelled, or a different device may have redeemed the link. So the
// deadline spends ONE rotation to learn which — the same rotation Ticket
// would have made — and reports what it says.
func (c *Client) AwaitActivation(ctx context.Context) error {
	deadline := c.now().Add(probeDeadline)
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		retired := c.retired
		c.mu.Unlock()
		if retired {
			return ErrSessionEnded
		}
		// Every failure is waited through, refusal and transport error
		// alike. A refusal here is the pending one by construction — the
		// route answers 404 for "not admitted yet" — and a transport
		// error says nothing about the pairing at all: the backend may be
		// mid-restart, or the network may have blinked. The deadline is
		// the bound on both.
		if _, err := c.mintTicket(ctx); err == nil {
			return nil
		}
		if !c.now().Before(deadline) {
			// One rotation, for the reason above. It either names the
			// refusal and forgets the session, or reports that the
			// pairing is still merely unconfirmed.
			if err := c.renew(ctx); err != nil {
				return err
			}
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// SetNickname records what this installation calls the backend, or clears
// it when the nickname is empty.
//
// Written through the client rather than by a caller editing the file,
// because a rotation is replacing that same file whenever the credential
// comes due: a write that raced one would either lose the nickname or
// roll the credential back to a refresh secret the backend has already
// spent. Taking the same lock rotate does makes the two orderly.
func (c *Client) SetNickname(nickname string) error {
	ctx, cancel := context.WithTimeout(context.Background(), profileWriteTimeout)
	defer cancel()
	return c.sessionTransaction(ctx, func(path string, latest *Session) error {
		latest.Nickname = nickname
		return writeSession(path, *latest)
	})
}

// Forget retires this owner and deletes only the pairing it still owns.
func (c *Client) Forget() error {
	ctx, cancel := context.WithTimeout(context.Background(), profileWriteTimeout)
	defer cancel()
	err := c.sessionTransaction(ctx, func(path string, latest *Session) error {
		if err := removeSessionFile(path); err != nil {
			return err
		}
		c.retired = true
		*latest = Session{BackendID: latest.BackendID, Endpoint: latest.Endpoint}
		return nil
	})
	if errors.Is(err, ErrSessionEnded) {
		return nil
	}
	return err
}

// Retire closes this in-memory owner before a replacement writes the same
// profile. It preserves the file so a failed new pairing can still recover.
func (c *Client) Retire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retired = true
	c.session = Session{BackendID: c.session.BackendID, Endpoint: c.session.Endpoint}
	if c.routes != nil {
		c.routes.CloseIdleConnections()
	}
}

// mintTicket performs one /auth/ticket exchange with a fresh proof.
func (c *Client) mintTicket(ctx context.Context) (string, error) {
	req, err := c.request(ctx, http.MethodPost, authTicketPath, nil)
	if err != nil {
		return "", err
	}
	if err := c.Authorize(req); err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("deviceclient: mint a socket ticket: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// This route answers the unfingerprintable 404 rather than a
		// typed reason, by design: a caller that names no live session
		// has nothing to bind a ticket to, and saying which of the
		// reasons applied would distinguish "this backend has sessions"
		// from "this path does not exist". The caller disambiguates by
		// rotating, not by reading a code that is not there.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		if err := temporaryHTTPFailure(resp.StatusCode); err != nil {
			return "", err
		}
		return "", &Refusal{Status: resp.StatusCode}
	}
	var minted struct {
		Ticket string `json:"ticket"`
	}
	if err := decodeBody(resp.Body, &minted); err != nil {
		return "", err
	}
	if minted.Ticket == "" {
		return "", errors.New("deviceclient: the backend answered a socket ticket with no ticket in it")
	}
	c.mu.Lock()
	knownRecovery := c.session.RefreshRecovery != nil && *c.session.RefreshRecovery
	c.mu.Unlock()
	if !knownRecovery && resp.Header.Get(RefreshRecoveryHeader) == "1" {
		if err := c.sessionTransaction(ctx, func(path string, latest *Session) error {
			if latest.RefreshRecovery != nil && *latest.RefreshRecovery {
				return nil
			}
			supported := true
			latest.RefreshRecovery = &supported
			return writeSession(path, *latest)
		}); err != nil {
			return "", err
		}
	}
	return minted.Ticket, nil
}

// renewIfStale rotates only when the held credential is close enough to
// expiry that the next call could be refused for age alone.
func (c *Client) renewIfStale(ctx context.Context) error {
	c.mu.Lock()
	expiresAt := c.session.ExpiresAtMs
	c.mu.Unlock()
	if expiresAt <= 0 || c.now().Add(renewMargin).UnixMilli() < expiresAt {
		return nil
	}
	return c.renew(ctx)
}

// renew coalesces callers in this process. The profile transaction and saved
// successor also coordinate independent processes; see refresh_recovery.go.
func (c *Client) renew(ctx context.Context) error {
	c.mu.Lock()
	if c.retired {
		c.mu.Unlock()
		return ErrSessionEnded
	}
	if flight := c.renewing; flight != nil {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	held := c.session
	flight := &renewal{done: make(chan struct{})}
	c.renewing = flight
	c.mu.Unlock()

	flight.err = c.rotate(ctx, held)

	c.mu.Lock()
	c.renewing = nil
	c.mu.Unlock()
	close(flight.done)
	return flight.err
}

// maxResponseBytes bounds one credential response. These bodies carry a
// credential, two timestamps and a short list of scope names; the cap is
// what stops a wedged or misconfigured endpoint making this process
// allocate on its behalf.
const maxResponseBytes = 64 << 10

// exchange performs one credential-issuing POST and reads the grant or the
// typed refusal out of it.
//
// The session headers are attached only when `held` is non-nil, which is
// exactly the difference between the two routes: a redemption presents a
// link token and a proof and nothing else, while a rotation presents the
// secret in the body and its proof in the header. Neither ever puts a
// proof in the body — a proof a caller may write into the same document it
// is proving something about is not a proof.
func (c *Client) exchange(ctx context.Context, path string, body []byte, held *Session) (grant, error) {
	req, err := c.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return grant{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	proof, err := mintProof(c.key, req.Method, path, c.now())
	if err != nil {
		return grant{}, err
	}
	req.Header.Set(DeviceKeyHeader, proof)
	if held != nil && held.Credential != "" {
		req.Header.Set(SessionCredentialHeader, held.Credential)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return grant{}, fmt.Errorf("deviceclient: reach %s%s: %w", c.base, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var answered grant
	if err := decodeBody(resp.Body, &answered); err != nil && resp.StatusCode == http.StatusOK {
		return grant{}, err
	}
	if err := temporaryHTTPFailure(resp.StatusCode); err != nil {
		return grant{}, err
	}
	if resp.StatusCode != http.StatusOK || answered.SessionID == "" || answered.Credential == "" {
		if answered.Reason == "" {
			return grant{}, fmt.Errorf("deviceclient: backend returned an invalid credential response (HTTP %d)", resp.StatusCode)
		}
		return grant{}, &Refusal{Status: resp.StatusCode, Reason: answered.Reason}
	}
	answered.refreshRecovery = resp.Header.Get(RefreshRecoveryHeader) == "1"
	return answered, nil
}

// Only an authentication verdict can end a stored pairing. A busy backend,
// proxy failure or rate limit is not evidence that the device was revoked.
func temporaryHTTPFailure(status int) error {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		return fmt.Errorf("deviceclient: backend temporarily unavailable (HTTP %d); try again", status)
	}
	return nil
}

// request builds one bounded request against this backend.
func (c *Client) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("deviceclient: build a request for %s: %w", path, err)
	}
	// No Origin header, deliberately. This is a client that is not a
	// browser, and the credential routes admit a request that names no
	// origin — one that named a fabricated one would be refused, correctly.
	return req, nil
}

// decodeBody reads one bounded JSON document.
func decodeBody(body io.Reader, into any) error {
	if err := json.NewDecoder(io.LimitReader(body, maxResponseBytes)).Decode(into); err != nil {
		return fmt.Errorf("deviceclient: read the backend's answer: %w", err)
	}
	return nil
}

package deviceclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/servercert"
)

// backend is a stand-in for the credential routes: it answers the three
// POSTs with the shapes internal/transport publishes, and records what it
// was presented. Enough of the wire to exercise this client's rules, and
// deliberately not a second implementation of the backend's — the real
// round trip is pinned in internal/app.
type backend struct {
	*httptest.Server

	mu sync.Mutex
	// spent records refresh secrets this backend has already rotated, so
	// presenting one twice reads as reuse the way the real one does.
	spent      map[string]bool
	credential string
	// refusal, when set, is what /auth/token answers instead of rotating.
	refusal string
	// ticketRefusals counts down mints that answer the unfingerprintable
	// 404 before the route starts working.
	ticketRefusals atomic.Int32
	tickets        atomic.Int32
	rotations      atomic.Int32
	// proofs is every X-AO-Device-Key value this backend was presented,
	// so a test can see that a proof was minted per request.
	proofs []string
}

func newBackend(t *testing.T) *backend {
	t.Helper()
	be := &backend{spent: map[string]bool{}, credential: "ao1.issued-0"}
	be.Server = httptest.NewServer(http.HandlerFunc(be.route))
	t.Cleanup(be.Close)
	return be
}

func (b *backend) route(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.proofs = append(b.proofs, r.Header.Get("X-AO-Device-Key"))
	b.mu.Unlock()

	switch r.URL.Path {
	case "/auth/pair":
		b.grant(w, "pending-verification")
	case "/auth/token":
		b.rotate(w, r)
	case "/auth/ticket":
		if b.ticketRefusals.Add(-1) >= 0 {
			http.NotFound(w, r)
			return
		}
		b.tickets.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "socket-ticket"})
	default:
		http.NotFound(w, r)
	}
}

func (b *backend) rotate(w http.ResponseWriter, r *http.Request) {
	b.rotations.Add(1)
	if b.refusal != "" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": b.refusal})
		return
	}
	var body struct {
		RefreshSecret string `json:"refreshSecret"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	b.mu.Lock()
	reused := b.spent[body.RefreshSecret]
	b.spent[body.RefreshSecret] = true
	b.mu.Unlock()
	if reused {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": "refresh_reused"})
		return
	}
	b.grant(w, "")
}

func (b *backend) grant(w http.ResponseWriter, verification string) {
	issued := int(b.rotations.Load())
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sessionId":          "session-1",
		"credential":         "ao1.issued-" + strconv.Itoa(issued),
		"expiresAtMs":        time.Now().Add(time.Hour).UnixMilli(),
		"refreshSecret":      "refresh-" + strconv.Itoa(issued+1),
		"refreshExpiresAtMs": time.Now().Add(24 * time.Hour).UnixMilli(),
		"verificationNumber": verification,
		"scopes":             []string{"threads:read"},
	})
}

func (b *backend) presentedProofs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.proofs...)
}

// openAgainst builds a client holding a live session on a test backend.
func openAgainst(t *testing.T, be *backend, mutate func(*Session)) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := EnrollDeviceKey(dir); err != nil {
		t.Fatalf("EnrollDeviceKey: %v", err)
	}
	session := Session{
		BackendID:          "backend-a",
		Endpoint:           be.URL,
		SessionID:          "session-1",
		Credential:         "ao1.issued-0",
		ExpiresAtMs:        time.Now().Add(time.Hour).UnixMilli(),
		RefreshSecret:      "refresh-0",
		RefreshExpiresAtMs: time.Now().Add(24 * time.Hour).UnixMilli(),
	}
	if mutate != nil {
		mutate(&session)
	}
	if err := SaveSession(dir, session); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	client, err := Open(dir, session)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return client, dir
}

// TestAuthorize_MintsAProofForTheRequestItRidesOn — a proof binds the
// method and the path and is spent on first use, so one prepared once and
// reused is refused as a replay and one carried to another route as not
// bound. Two calls therefore produce two proofs.
func TestAuthorize_MintsAProofForTheRequestItRidesOn(t *testing.T) {
	be := newBackend(t)
	client, _ := openAgainst(t, be, nil)
	ctx := context.Background()

	if _, err := client.Ticket(ctx); err != nil {
		t.Fatalf("Ticket: %v", err)
	}
	if _, err := client.Ticket(ctx); err != nil {
		t.Fatalf("Ticket again: %v", err)
	}
	proofs := be.presentedProofs()
	if len(proofs) != 2 {
		t.Fatalf("the backend saw %d requests, want 2", len(proofs))
	}
	if proofs[0] == proofs[1] {
		t.Fatal("both mints presented the same proof; a proof is spent by its first use")
	}
	for _, proof := range proofs {
		if strings.Count(proof, ".") != 2 {
			t.Fatalf("proof %q is not a compact JWS", proof)
		}
	}
}

// TestAuthorize_RefusesWithNoSession — a client whose session was cleared
// must not present an empty credential header, which the backend would
// read as a request naming no session at all.
func TestAuthorize_RefusesWithNoSession(t *testing.T) {
	be := newBackend(t)
	client, _ := openAgainst(t, be, nil)
	if err := client.Forget(); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, be.URL+"/auth/ticket", nil)
	if err != nil {
		t.Fatalf("build a request: %v", err)
	}
	if err := client.Authorize(req); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Authorize with no session = %v, want ErrNoSession", err)
	}
}

// TestTicket_RenewsOnceAndRetriesWithAFreshProof — /auth/ticket answers
// the same unfingerprintable 404 for "not admitted yet" and "not admitted
// any more", so one rotation tells them apart. The retry must mint a new
// proof: the first attempt's is spent.
func TestTicket_RenewsOnceAndRetriesWithAFreshProof(t *testing.T) {
	be := newBackend(t)
	be.ticketRefusals.Store(1)
	client, _ := openAgainst(t, be, nil)

	if _, err := client.Ticket(context.Background()); err != nil {
		t.Fatalf("Ticket: %v", err)
	}
	if be.rotations.Load() != 1 {
		t.Fatalf("rotations = %d, want exactly one", be.rotations.Load())
	}
	proofs := be.presentedProofs()
	if len(proofs) != 3 {
		t.Fatalf("the backend saw %d requests, want mint, rotate, mint", len(proofs))
	}
	if proofs[0] == proofs[2] {
		t.Fatal("the retry replayed the first attempt's proof")
	}
}

// TestTicket_RotatesBeforeAStaleCredentialIsUsed — a ticket minted with a
// credential about to lapse could outlive it between the mint and the
// upgrade it is for, so the rotation happens first rather than after a
// refusal.
func TestTicket_RotatesBeforeAStaleCredentialIsUsed(t *testing.T) {
	be := newBackend(t)
	client, dir := openAgainst(t, be, func(s *Session) {
		s.ExpiresAtMs = time.Now().Add(renewMargin / 2).UnixMilli()
	})

	if _, err := client.Ticket(context.Background()); err != nil {
		t.Fatalf("Ticket: %v", err)
	}
	if be.rotations.Load() != 1 {
		t.Fatalf("rotations = %d, want one before the mint", be.rotations.Load())
	}
	// Stored before it was used: nothing may present a credential this
	// process failed to write down.
	stored, err := LoadSession(dir, "backend-a")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if stored.Credential != client.Session().Credential {
		t.Fatalf("stored credential %q is not the one in memory %q", stored.Credential, client.Session().Credential)
	}
	if stored.RefreshSecret == "refresh-0" {
		t.Fatal("the stored refresh secret was not rotated")
	}
}

// TestRenew_IsSingleFlight is the reuse-detection rule in one test. Two
// concurrent rotations present the SAME secret, and the second reads as
// reuse evidence — which ends the session, every socket carrying it, and
// every outstanding secret in the chain. So concurrent callers must
// observe one exchange.
func TestRenew_IsSingleFlight(t *testing.T) {
	be := newBackend(t)
	client, _ := openAgainst(t, be, func(s *Session) {
		s.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	})

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = client.Ticket(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := be.rotations.Load(); got != 1 {
		t.Fatalf("rotations = %d, want exactly one for %d concurrent callers", got, callers)
	}
}

// TestRenew_ReuseEvidenceClearsTheSessionAndKeepsTheKey — presenting a
// spent secret ends the family. The stored session goes, so this process
// stops presenting something that can never succeed; the device key stays,
// because it names the DEVICE and the backend adopts its row by
// thumbprint when this installation pairs again.
func TestRenew_ReuseEvidenceClearsTheSessionAndKeepsTheKey(t *testing.T) {
	be := newBackend(t)
	be.refusal = "refresh_reused"
	client, dir := openAgainst(t, be, func(s *Session) {
		s.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	})

	_, err := client.Ticket(context.Background())
	if !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("Ticket after reuse = %v, want ErrSessionEnded", err)
	}
	if !strings.Contains(err.Error(), "refresh_reused") {
		t.Errorf("the error drops the reason the audit log keeps: %v", err)
	}
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("the refused session survived: %v", err)
	}
	if _, err := DeviceKey(dir); err != nil {
		t.Fatalf("the device key did not survive a refusal: %v", err)
	}
}

// TestRenew_PendingConfirmationKeepsTheSession — this is the one refusal
// where presenting the same credential again is expected to succeed
// shortly, so forgetting it would throw away a pairing the owner is about
// to confirm.
func TestRenew_PendingConfirmationKeepsTheSession(t *testing.T) {
	be := newBackend(t)
	be.refusal = reasonPendingConfirmation
	client, dir := openAgainst(t, be, func(s *Session) {
		s.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	})

	if _, err := client.Ticket(context.Background()); !errors.Is(err, ErrAwaitingConfirmation) {
		t.Fatalf("Ticket while pending = %v, want ErrAwaitingConfirmation", err)
	}
	if _, err := LoadSession(dir, "backend-a"); err != nil {
		t.Fatalf("the pending session was forgotten: %v", err)
	}
}

// TestRenew_NeverRetriesAnUnreadExchange — a request whose response never
// arrived may or may not have spent the secret, and this client cannot
// tell that from a copy any better than the backend can. So a transport
// failure is reported as itself and the stored session is left alone.
func TestRenew_NeverRetriesAnUnreadExchange(t *testing.T) {
	be := newBackend(t)
	client, dir := openAgainst(t, be, func(s *Session) {
		s.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	})
	be.Close()

	err := client.renew(context.Background())
	if err == nil {
		t.Fatal("renew against a dead backend succeeded")
	}
	if errors.Is(err, ErrSessionEnded) {
		t.Fatalf("an unreachable backend was read as a refusal: %v", err)
	}
	stored, err := LoadSession(dir, "backend-a")
	if err != nil {
		t.Fatalf("the stored session was cleared by a transport failure: %v", err)
	}
	if stored.RefreshSecret != "refresh-0" {
		t.Fatalf("the stored secret changed on an unread exchange: %q", stored.RefreshSecret)
	}
}

// TestRotate_WithNoSecretIsFinishedRatherThanRetried — a session with no
// refresh secret cannot renew and never will, which is either a binding
// class that does not rotate or a store this client already cleared.
func TestRotate_WithNoSecretIsFinishedRatherThanRetried(t *testing.T) {
	be := newBackend(t)
	client, _ := openAgainst(t, be, func(s *Session) {
		s.RefreshSecret = ""
		s.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	})
	if err := client.renew(context.Background()); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("renew with no secret = %v, want ErrSessionEnded", err)
	}
	if be.rotations.Load() != 0 {
		t.Fatal("a session with no secret still made a round trip")
	}
}

// TestPinnedTransport_RefusesAnotherCertificateBeforeAnythingIsSent is the
// property the pairing ceremony bought. The pin is compared during the
// handshake, so a backend presenting different bytes never receives this
// device's credential at all.
func TestPinnedTransport_RefusesAnotherCertificateBeforeAnythingIsSent(t *testing.T) {
	reached := atomic.Bool{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	presented := upstream.Certificate().Raw
	client := &http.Client{Transport: pinnedTransport("sha256:" + strings.Repeat("00", 32))}
	_, err := client.Get(upstream.URL)
	if !errors.Is(err, ErrCertificateMismatch) {
		t.Fatalf("a mismatched pin = %v, want ErrCertificateMismatch", err)
	}
	if reached.Load() {
		t.Fatal("the request reached the backend despite the pin refusing it")
	}
	if !strings.Contains(err.Error(), servercert.Fingerprint(presented)) {
		t.Errorf("the refusal does not name what was presented: %v", err)
	}

	// The matching pin connects, which is what makes the refusal above a
	// judgement about the certificate rather than about TLS in general.
	client = &http.Client{Transport: pinnedTransport(servercert.Fingerprint(presented))}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("the matching pin refused a good certificate: %v", err)
	}
	_ = resp.Body.Close()
}

// TestPinnedTLSConfig_NoFingerprintIsOrdinaryWebPKI — the empty case is
// the OTHER supported posture, not an absence of one. There is
// deliberately no third state in which this client is encrypted and
// verifying nothing.
func TestPinnedTLSConfig_NoFingerprintIsOrdinaryWebPKI(t *testing.T) {
	if cfg := pinnedTLSConfig(""); cfg != nil {
		t.Fatalf("an empty fingerprint produced %+v, want nil (ordinary verification)", cfg)
	}
	cfg := pinnedTLSConfig("sha256:abcd")
	if cfg == nil || !cfg.InsecureSkipVerify || cfg.VerifyPeerCertificate == nil {
		t.Fatalf("a pinned config is %+v, want skip-verify plus the stricter check", cfg)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want the backend listener's floor", cfg.MinVersion)
	}
	if err := cfg.VerifyPeerCertificate(nil, nil); !errors.Is(err, ErrCertificateMismatch) {
		t.Errorf("a peer presenting no certificate = %v, want a mismatch", err)
	}
}

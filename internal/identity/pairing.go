package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// Pairing: how a device this backend has never seen becomes one it has
// (docs/specs/remote-access.md §4 "Pairing").
//
// Four facts hold the flow together, and each is here because removing it
// would leave a hole the others do not cover:
//
//  1. The link is single-use with a five-minute window, and consumption is
//     one CAS statement in the store. A link seen twice admits one device.
//  2. The NEW device generates its key first and presents the thumbprint
//     during redemption. Proof-of-possession is universal, not peer-only:
//     a link read off a screen or out of a chat log buys nothing without
//     the key that redeemed it — and cannot be redeemed a second time by
//     a different key, because the first redemption spent it.
//  3. The owner confirms a short verification number derived from that
//     device's key before the session activates. This is what closes the
//     silent-race case: if some other device redeemed the link first, the
//     number the minting surface shows is derived from ITS key and will
//     not match the number on the device in the owner's hand.
//  4. A link may carry a scope subset, so a viewer link or a peer
//     invitation is the same mechanism with a narrower grant rather than a
//     second flow.
//
// The minting, confirming, and cancelling calls are host-side Go API: they
// are reached from an authenticated admin surface (internal/app's
// app_access.go, docs/specs/remote-access.md §4 step 1) whose RPCs all
// carry `//ao:scope access:admin`, and MINTING additionally carries
// `//ao:stepup` — so a link can only be created by somebody at the
// machine, whatever a session was granted. Only the device-facing half —
// redemption — crosses the network as an unauthenticated route, because
// only it is spoken by something that holds no credential yet.

const (
	// pairingTokenBytes is the entropy in a link token: 32 bytes, the same
	// budget as the launch token, encoded base64url without padding.
	pairingTokenBytes = 32

	// pairingHashDomain separates a link-token digest from every other
	// SHA-256 in this package.
	pairingHashDomain = "agent-overflow/pairing-token/v1\x00"

	// verificationDomain separates the verification-number MAC.
	verificationDomain = "agent-overflow/pairing-verification/v1\x00"

	// verificationDigits is how many decimal digits the owner compares.
	// Six is what a person will actually read off one screen and check
	// against another, and the window it has to be guessed inside is the
	// pairing TTL of a link that has already been redeemed once.
	verificationDigits = 6

	// PairingPayloadVersion versions the fragment payload. A second
	// version is a different number and a different parse, never a flag
	// inside the same shape.
	PairingPayloadVersion = 1
)

// ErrPairingRefused is returned for a pairing link that admits nothing: no
// such token, a spent one, an expired one, or a canceled one. A caller
// cannot tell them apart, and neither should the device presenting it —
// the difference is a fact about this backend's records, not about the
// request.
var ErrPairingRefused = errors.New("identity: pairing refused")

// ErrPairingNotCompleted answers a confirmation that could not admit its
// device; the link was cancelled and its session revoked, so the device has
// to pair again.
var ErrPairingNotCompleted = errors.New("identity: the pairing was not completed; pair the device again")

// PairingPayload is what the minting surface hands to the new device: the
// contents of the URL fragment behind a pairing link, and the same fields a
// QR code or a typed code carries.
//
// A fragment, not a query: a fragment is never sent to a server, never
// written to an access log, and never lands in a Referer header.
//
// Additive-only, like every other shape on this wire. A field may be
// appended; none may change meaning.
type PairingPayload struct {
	Purpose string `json:"purpose,omitempty"`
	// Version is PairingPayloadVersion. A device that does not recognise
	// it refuses rather than guessing at the rest.
	Version int `json:"v"`
	// BackendID names the backend that minted this link, so a device that
	// already knows several can say which one is offering to pair.
	BackendID string `json:"backendId"`
	// BackendName is the display name shown while pairing. Convenience
	// only: it grants nothing and is never matched against anything.
	BackendName string `json:"backendName,omitempty"`
	// Endpoint is the base URL to redeem against.
	Endpoint string `json:"endpoint"`
	// Token is the single-use link token.
	Token string `json:"token"`
	// CertFingerprint is the backend's TLS certificate fingerprint, which a
	// native client pins for the redemption exchange and from then on (§4
	// step 6, §7). "sha256:<lowercase hex>" over the DER of the leaf, as
	// internal/servercert renders it.
	//
	// This package is fingerprint-AGNOSTIC: it carries whatever the
	// minting surface handed it and compares nothing. Only the boot knows
	// which certificate its listener presents (internal/app's
	// MintDevicePairing reads it from there), so a value invented here
	// would be a second answer to a question this package cannot see.
	//
	// Empty delegates to ordinary WebPKI verification in native clients;
	// it never means skipping TLS checks. Invitation trust comes from its
	// delivery (QR/link or the committed pairbootstrap exchange), not from
	// repeating the server-returned verification number over untrusted TLS.
	CertFingerprint string `json:"certFingerprint,omitempty"`
}

// Encode renders the payload for a URL fragment: compact JSON, base64url,
// no padding. Padding is dropped because `=` in a fragment is legal but
// survives copy-paste badly.
func (p PairingPayload) Encode() (string, error) {
	buf, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("identity: encode pairing payload: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// DecodePairingPayload parses what Encode produced. A payload naming a
// version this build does not know is refused rather than partially read:
// the fields it would keep are exactly the ones a version bump would have
// changed the meaning of.
func DecodePairingPayload(encoded string) (PairingPayload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, "#"))
	if err != nil {
		return PairingPayload{}, fmt.Errorf("identity: decode pairing payload: %w", err)
	}
	var payload PairingPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return PairingPayload{}, fmt.Errorf("identity: decode pairing payload: %w", err)
	}
	if payload.Version != PairingPayloadVersion {
		return PairingPayload{}, fmt.Errorf(
			"identity: pairing payload version %d is not %d", payload.Version, PairingPayloadVersion)
	}
	if payload.Token == "" {
		return PairingPayload{}, fmt.Errorf("identity: pairing payload carries no token")
	}
	return payload, nil
}

// PairingRequest describes a link to mint.
type PairingRequest struct {
	Purpose           string
	ExpectedKey       string
	MemberGeneration  int64
	SponsorKey        string
	SponsorGeneration int64
	// UserID is the account the redeemed device binds to. Explicit, never
	// "the owner": a hub deployment mints links for many accounts (§11).
	UserID string
	// DeviceClass and BindingClass are decided HERE, by the minting
	// surface, and not by the redeeming device. A device that could name
	// its own binding class could name `loopback-only` and inherit the
	// local channel's posture.
	DeviceClass  DeviceClass
	BindingClass BindingClass
	// Scopes is the subset this link grants, and the set the redeemed
	// session holds for its whole life. A viewer link and a peer
	// invitation are this field, not a second flow — PairingAccess is the
	// device-pairing surface's name for the two choices it offers.
	Scopes []Scope
	// CertFingerprint is what the redeemed device pins for this backend's
	// TLS (see PairingPayload). Supplied by the minting surface, which is
	// the only layer that knows which certificate the listener presents;
	// empty is a link that names none.
	CertFingerprint string
}

// PairingLink is a minted, unredeemed link: the row plus the token, which
// is returned exactly once and never stored.
type PairingLink struct {
	Link store.PairingLink
	// Token is the value the payload carries. The store holds only its
	// digest, so this string cannot be recovered after this return.
	Token string
}

// MintPairingLink issues a single-use link. Host-side: the caller is an
// authenticated admin surface in this process, and the step-up requirement
// on that call is phase 3's (§4 "Step-up").
func (s *Sessions) MintPairingLink(req PairingRequest) (PairingLink, error) {
	if req.Purpose != "" && req.Purpose != "own-device" {
		return PairingLink{}, errors.New("restricted introductions require member authorization")
	}
	return s.mintPairingLink(req)
}
func (s *Sessions) mintPairingLink(req PairingRequest) (PairingLink, error) {
	if strings.TrimSpace(req.UserID) == "" {
		return PairingLink{}, fmt.Errorf("identity: mint pairing link: user id is required")
	}
	if !req.DeviceClass.Valid() {
		return PairingLink{}, fmt.Errorf("identity: %q is not a declared device class", string(req.DeviceClass))
	}
	if !req.BindingClass.Valid() || req.BindingClass == BindingLoopbackOnly {
		return PairingLink{}, fmt.Errorf("identity: %q is not a declared binding class", string(req.BindingClass))
	}
	scopes, err := ValidateScopes(req.Scopes)
	if err != nil {
		return PairingLink{}, err
	}
	token, digest, err := newPairingToken()
	if err != nil {
		return PairingLink{}, err
	}
	now := s.now().UnixMilli()
	link := store.PairingLink{
		Purpose: req.Purpose, ExpectedKey: req.ExpectedKey, MemberGeneration: req.MemberGeneration, SponsorKey: req.SponsorKey,
		ID:              uuid.NewString(),
		UserID:          req.UserID,
		Scopes:          scopes,
		BindingClass:    string(req.BindingClass),
		DeviceClass:     string(req.DeviceClass),
		CertFingerprint: req.CertFingerprint,
		CreatedAt:       now,
		ExpiresAt:       now + PairingLinkTTL.Milliseconds(),
	}
	if err := s.store.CreatePairingLink(link, digest[:]); err != nil {
		return PairingLink{}, err
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPairingLinkMinted), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: req.UserID, Detail: fmt.Sprintf("%s/%s", req.DeviceClass, req.BindingClass),
	})
	return PairingLink{Link: link, Token: token}, nil
}

// RedemptionRequest is what a new device presents.
type RedemptionRequest struct {
	// Token is the link token from the payload fragment.
	Token string
	// Proof is what the device generated BEFORE this call and presents to
	// enroll. REQUIRED on every redemption: proof-of-possession is
	// universal (§4 step 2), so there is no path where a link alone admits
	// a device.
	//
	// Two shapes, and the device chooses which by what it can do:
	//
	//   - a signed proof over this redemption, which enrolls the device
	//     `key`. The public key travels inside it, so the thumbprint this
	//     backend records is derived from a key the device just
	//     DEMONSTRATED it holds — enrollment is itself a possession proof,
	//     not a claim to be trusted later.
	//   - a bare opaque identifier, which enrolls it `bearer`. This is the
	//     plain-HTTP LAN browser of §15 constraint 6: no secure context,
	//     no WebCrypto, nothing to sign with.
	//
	// A device may only make itself weaker by choosing the second, which
	// is why sniffing the shape is safe HERE and refused everywhere else:
	// at enrollment there is no stronger row to downgrade from.
	Proof DeviceProof
	// Label and Platform are what the device calls itself. Presentation
	// only — nothing authorizes on either, which is why the device is
	// allowed to name them and not its class.
	Label    string
	Platform string
	// Peer is the request's source address, for audit attribution.
	Peer string
}

// Redemption is what a redeeming device gets back.
type Redemption struct {
	// PairingID names the link, so the minting surface and the device are
	// talking about the same exchange.
	PairingID string
	// DeviceID is the device row this redemption resolved to — new, or the
	// existing row belonging to the same key.
	DeviceID string
	// Tokens is the real credential pair, issued immediately and inert
	// until confirmation (Tokens.AwaitingConfirmation).
	Tokens TokenSet
	// VerificationNumber is the digits the owner compares. The device
	// displays it; the minting surface derives the same value from the
	// thumbprint that actually redeemed.
	VerificationNumber string
}

// RedeemPairing consumes a link on behalf of a new device.
//
// The credential pair is issued HERE, before the owner confirms, and is
// inert until they do — `sessions.activated_at` is NULL, so the same
// predicate that refuses a revoked session refuses this one. That is why
// there is no second secret and no polling endpoint: the device holds its
// real credential and simply keeps presenting it, and the moment the owner
// confirms, the next presentation is admitted.
//
// A key this backend already knows resolves to its EXISTING device row
// rather than a second one — re-pairing a phone after a wipe is the same
// physical device, and the thumbprint column is uniquely indexed precisely
// so one key can never name two. A key naming a REVOKED device is refused:
// re-admitting it would let a fresh link undo a revocation, and the
// deliberate answer to "I want that device back" is to remove the
// revocation on the device surface.
func (s *Sessions) RedeemPairing(req RedemptionRequest) (Redemption, Reason) {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if req.Token == "" || req.Proof.Value == "" {
		return Redemption{}, ReasonMissingProof
	}
	// Resolve the enrollment BEFORE the link is spent. A signed proof that
	// does not verify must not cost the link somebody minted: nothing has
	// happened yet, and cancelling here would make a client-side signing
	// bug indistinguishable from a spent link.
	enrollment, reason := s.enrollmentFor(req.Proof)
	if reason.Refused() {
		s.auditPairingRefusal(reason, req.Peer, "")
		return Redemption{}, reason
	}
	digest := hashPairingToken(req.Token)
	now := s.now().UnixMilli()
	checked, err := s.store.PairingLinkByTokenHash(digest[:])
	if err != nil {
		s.auditPairingRefusal(ReasonUnknownCredential, req.Peer, "")
		return Redemption{}, ReasonUnknownCredential
	}
	if checked.Purpose != "" && enrollment.kind != ProofSignedKey {
		s.auditPairingRefusal(ReasonMissingProof, req.Peer, checked.ID)
		return Redemption{}, ReasonMissingProof
	}
	if checked.ExpectedKey != "" && checked.ExpectedKey != enrollment.thumbprint {
		s.auditPairingRefusal(ReasonKeyMismatch, req.Peer, checked.ID)
		return Redemption{}, ReasonKeyMismatch
	}
	if checked.Purpose == "own-introduction" && !s.ownIntroductionLive(checked) {
		s.auditPairingRefusal(ReasonUnknownCredential, req.Peer, checked.ID)
		return Redemption{}, ReasonUnknownCredential
	}
	// The number depends only on the link and the key, so it is derived
	// before anything is written: a redemption that commits is never then
	// refused.
	number, err := s.VerificationNumber(checked.ID, enrollment.thumbprint)
	if err != nil {
		log.Printf("identity: derive verification number for %s: %v", checked.ID, err)
		return Redemption{}, ReasonUnknownCredential
	}

	// Spend the link FIRST. Everything after this point can still refuse
	// the redemption, and a refusal settles the link (cancelRedeemedLink)
	// rather than releasing it — a link that could be freed by a failed
	// redemption would be one a second presentation gets another turn at,
	// which is exactly what single-use forbids.
	pending, err := s.store.RedeemPairingLink(digest[:], now, enrollment.thumbprint)
	if errors.Is(err, sql.ErrNoRows) {
		s.auditPairingRefusal(ReasonUnknownCredential, req.Peer, "")
		return Redemption{}, ReasonUnknownCredential
	}
	if err != nil {
		log.Printf("identity: redeem pairing link: %v", err)
		return Redemption{}, ReasonUnknownCredential
	}

	device, newDevice, reason := s.resolveDevice(pending.UserID, DeviceClass(pending.DeviceClass), req.Label, req.Platform, enrollment)
	if reason.Refused() {
		s.cancelRedeemedLink(pending.ID)
		s.auditPairingRefusal(reason, req.Peer, pending.ID)
		return Redemption{}, reason
	}

	// The link names what it produced before the device is told the
	// redemption worked: the device row, the session, its refresh secret,
	// the link's record of them and, for an own-device introduction that no
	// owner confirms, the confirmation commit together or not at all. A
	// failure here leaves none of them, and the spent link is settled so
	// nothing can present it again.
	session, tokens, err := s.mintRedemptionSession(pending, device, newDevice, now)
	if err != nil {
		log.Printf("identity: mint pairing session for link %s: %v", pending.ID, err)
		s.cancelRedeemedLink(pending.ID)
		if errors.Is(err, store.ErrPairingIncomplete) {
			// The introduction's membership was refused: recorded as the
			// cancellation it is.
			s.auditPairing(AuditPairingCanceled, pending.UserID, device.ID, "", pending.ID)
		}
		return Redemption{}, ReasonUnknownCredential
	}

	s.audit(store.AuthAuditEntry{
		Event: string(AuditPairingRedeemed), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: pending.UserID, DeviceID: device.ID, SessionID: session.ID, Peer: req.Peer,
		Detail: pending.ID,
	})
	return Redemption{
		PairingID:          pending.ID,
		DeviceID:           device.ID,
		Tokens:             tokens,
		VerificationNumber: number,
	}, ReasonNone
}

// deviceEnrollment is what a redemption's proof resolved to: the
// thumbprint that will name the device row, and how that thumbprint may be
// presented from then on.
type deviceEnrollment struct {
	thumbprint string
	kind       ProofKind
}

// enrollmentFor reads a redemption's presentation and decides which of the
// two enrollments it is.
//
// The signed branch VERIFIES before it records anything. That is what
// makes a `key` device's thumbprint trustworthy for the whole life of the
// row: it is the hash of a key whose private half signed this exact
// redemption, checked here in the same order CheckDeviceProof uses —
// signature, then binding, then freshness, then replay — and there is no
// device row yet to compare a thumbprint against, so that step is simply
// absent rather than skipped.
//
// The bearer branch records the identifier as presented. It proves nothing
// beyond "the same device will send this string again", which is exactly
// what §15 constraint 6 leaves available to a page with no WebCrypto, and
// exactly what the owner-confirmed verification number is there to
// compensate for: that number is derived from the thumbprint, so a device
// that enrolled a different one cannot display digits the owner's screen
// will match.
func (s *Sessions) enrollmentFor(presented DeviceProof) (deviceEnrollment, Reason) {
	if !presented.Signed() {
		return deviceEnrollment{thumbprint: presented.Value, kind: ProofBearer}, ReasonNone
	}
	parsed, reason := parseDeviceProof(presented.Value)
	if reason.Refused() {
		return deviceEnrollment{}, reason
	}
	if reason := s.admitProof(parsed, presented); reason.Refused() {
		return deviceEnrollment{}, reason
	}
	return deviceEnrollment{thumbprint: parsed.thumbprint, kind: ProofSignedKey}, ReasonNone
}

// resolveDevice finds the device row for a presented key, relabeled to what
// the device now reports, or builds a new one of class for userID; newDevice
// says which. It writes nothing: the row is written with the session it was
// resolved for (newDeviceSession). Pairing redemption and passkey sign-in
// both resolve here, so one rule answers "is this the same device". See
// RedeemPairing for why an existing key is adopted and a revoked one is
// refused.
func (s *Sessions) resolveDevice(userID string, class DeviceClass, label, platform string, enrollment deviceEnrollment) (device store.Device, newDevice bool, reason Reason) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = defaultDeviceLabel(class)
	}
	existing, err := s.store.DeviceByKeyThumbprint(enrollment.thumbprint)
	switch {
	case err == nil:
		if proofKindOf(existing) != enrollment.kind {
			// Adoption must never change what a row's thumbprint MEANS.
			// The reachable direction is a device whose row records an
			// enrolled key presenting that thumbprint as a bare
			// identifier — which would adopt the row and make every later
			// presentation a string comparison, undoing phase 5 for that
			// device permanently through the one path that writes device
			// rows. The other direction cannot occur: a key thumbprint is
			// the hash of a public key, so a bearer row's opaque
			// identifier does not collide with one.
			return store.Device{}, false, ReasonProofDowngraded
		}
		if existing.RevokedAt != 0 {
			// The owner withdrew this device; a fresh link must not undo
			// that by itself. The reason names the remedy: restore the
			// device on the owner surface, then redeem a NEW link.
			return store.Device{}, false, ReasonRevokedDevice
		}
		if existing.UserID != userID {
			return store.Device{}, false, ReasonKeyMismatch
		}
		existing.Label, existing.Platform = label, platform
		return existing, false, ReasonNone
	case !errors.Is(err, sql.ErrNoRows):
		log.Printf("identity: read device by thumbprint: %v", err)
		return store.Device{}, false, ReasonKeyMismatch
	}
	device, err = store.NewPairedDevice(
		userID, label, string(class), platform,
		enrollment.thumbprint, string(enrollment.kind))
	if err != nil {
		log.Printf("identity: build device: %v", err)
		return store.Device{}, false, ReasonKeyMismatch
	}
	return device, true, ReasonNone
}

// newDeviceSession builds the session req describes on device and its first
// credential pair, for the caller to write together with the device row
// (store.DeviceSession) before it hands the pair out, so a device never
// holds an access credential whose renewal row does not exist.
func (s *Sessions) newDeviceSession(req MintRequest, device store.Device, newDevice bool, now int64) (store.DeviceSession, TokenSet, error) {
	session, err := s.newSession(req, device.ID)
	if err != nil {
		return store.DeviceSession{}, TokenSet{}, err
	}
	tokens, grant, err := s.newTokens(session, device, now)
	if err != nil {
		return store.DeviceSession{}, TokenSet{}, err
	}
	return store.DeviceSession{Device: device, NewDevice: newDevice, Session: session, Refresh: grant}, tokens, nil
}

// mintRedemptionSession issues the unactivated session and its first
// credential pair, written with the device row (created or relabeled) and
// the link that names them in one store transaction. The window is the
// confirmation window, not the device class's access TTL: an unconfirmed
// credential must not outlive the decision it is waiting on. Confirm
// replaces it with the real one.
//
// An own-device introduction has no owner to confirm it, so the same
// transaction confirms it: the session is activated with the device class's
// access window and the membership admitted.
func (s *Sessions) mintRedemptionSession(link store.PairingLink, device store.Device, newDevice bool, now int64) (store.Session, TokenSet, error) {
	scopes := make([]Scope, 0, len(link.Scopes))
	for _, name := range link.Scopes {
		scopes = append(scopes, Scope(name))
	}
	written, tokens, err := s.newDeviceSession(MintRequest{
		UserID:            link.UserID,
		BindingClass:      BindingClass(link.BindingClass),
		Scopes:            scopes,
		TTL:               PairingConfirmWindow,
		AwaitConfirmation: true,
	}, device, newDevice, now)
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	redemption := store.PairingRedemption{LinkID: link.ID, DeviceSession: written}
	introduction := link.Purpose == "own-introduction"
	if introduction {
		redemption.ConfirmedAt = now
		redemption.AccessExpiresAt = now + RenewablePolicyFor(DeviceClass(device.Class)).Access.Milliseconds()
	}
	if err := s.store.CompletePairingRedemption(redemption); err != nil {
		return store.Session{}, TokenSet{}, err
	}
	s.auditMinted(written.Session)
	if introduction {
		tokens.AwaitingConfirmation = false
		s.auditPairing(AuditPairingConfirmed, link.UserID, device.ID, written.Session.ID, link.ID)
	}
	return written.Session, tokens, nil
}

// cancelRedeemedLink settles a link whose redemption could not complete, so
// the spent link cannot be presented again by anything.
func (s *Sessions) cancelRedeemedLink(linkID string) {
	if _, err := s.cancelLink(linkID); err != nil && !errors.Is(err, ErrPairingRefused) {
		log.Printf("identity: cancel pairing link %s: %v", linkID, err)
	}
}

// cancelLink ends a link and its redemption's pending session in one store
// transaction, then tears down what the revocation ended at runtime. Only
// a committed cancellation reaches the teardown.
func (s *Sessions) cancelLink(linkID string) (store.PairingLink, error) {
	canceled, err := s.store.CancelPairingLink(linkID, s.now().UnixMilli())
	if errors.Is(err, sql.ErrNoRows) {
		return store.PairingLink{}, ErrPairingRefused
	}
	if err != nil {
		return store.PairingLink{}, err
	}
	if canceled.Link.SessionID != "" {
		s.sessionRevoked(canceled.Link.SessionID, canceled.SessionRevoked, canceled.Released)
	}
	return canceled.Link, nil
}

// ConfirmPairing activates the session a redemption created, after the
// owner has matched the verification number.
//
// Host-side, like minting. Returns the link so the caller can report which
// device was admitted.
//
// The activation also replaces the confirmation window with the device
// class's real access window: until this moment the credential's only job
// was to exist, and its expiry was the deadline on the owner's decision.
func (s *Sessions) ConfirmPairing(linkID string) (store.PairingLink, error) {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	return s.confirmPairing(linkID)
}

// confirmPairing is ConfirmPairing with ownMu held.
//
// The confirmation, the session's activation and a personal pairing's
// membership commit together (store.ConfirmRedeemedPairing). A session that
// can no longer be activated, because it or its device was revoked or it
// lapsed, or a membership no longer admissible, cancels the link and
// revokes the session instead and answers ErrPairingNotCompleted. A
// storage failure changes nothing, so the owner can try again.
func (s *Sessions) confirmPairing(linkID string) (store.PairingLink, error) {
	pending, err := s.store.GetPairingLink(linkID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && pending.DeviceID == "") {
		return store.PairingLink{}, ErrPairingRefused
	}
	if err != nil {
		return store.PairingLink{}, err
	}
	// The device a redemption recorded never changes, so its class read
	// here is the class of the device the transaction admits.
	device, err := s.store.GetDevice(pending.DeviceID)
	if err != nil {
		return store.PairingLink{}, fmt.Errorf("identity: confirm pairing: read device: %w", err)
	}
	policy := RenewablePolicyFor(DeviceClass(device.Class))
	now := s.now().UnixMilli()
	confirmation, err := s.store.ConfirmRedeemedPairing(linkID, now, now+policy.Access.Milliseconds())
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store.PairingLink{}, ErrPairingRefused
	case errors.Is(err, store.ErrPairingIncomplete):
		canceled := confirmation.Canceled
		s.sessionRevoked(canceled.Link.SessionID, canceled.SessionRevoked, canceled.Released)
		s.auditPairing(AuditPairingCanceled, canceled.Link.UserID, canceled.Link.DeviceID, canceled.Link.SessionID, canceled.Link.ID)
		return store.PairingLink{}, fmt.Errorf("%w: %v", ErrPairingNotCompleted, err)
	case err != nil:
		return store.PairingLink{}, err
	}
	link := confirmation.Link
	s.auditPairing(AuditPairingConfirmed, link.UserID, link.DeviceID, link.SessionID, link.ID)
	return link, nil
}

// auditPairing records an owner-side decision on a pairing link.
func (s *Sessions) auditPairing(event AuditEvent, userID, deviceID, sessionID, linkID string) {
	s.audit(store.AuthAuditEntry{
		Event: string(event), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: userID, DeviceID: deviceID, SessionID: sessionID,
		Detail: linkID,
	})
}

// CancelPairing refuses a link — the verification number did not match, or
// it was minted by mistake — and revokes whatever a redemption already
// created.
//
// The link and its pending session end in one transaction, so a failure
// leaves both for a retry and an answer of ErrPairingRefused means both
// are settled. The link stops admitting anything in the same commit that
// revokes the session it created, so no redemption in flight can mint a
// second one behind the teardown.
func (s *Sessions) CancelPairing(linkID string) (store.PairingLink, error) {
	link, err := s.cancelLink(linkID)
	if err != nil {
		return store.PairingLink{}, err
	}
	s.auditPairing(AuditPairingCanceled, link.UserID, link.DeviceID, link.SessionID, link.ID)
	return link, nil
}

// PairingLink reads one link, for the minting surface to poll while it
// waits for a device to redeem. It carries no secret: the token is not
// stored. ErrPairingRefused when no such link exists.
func (s *Sessions) PairingLink(linkID string) (store.PairingLink, error) {
	link, err := s.store.GetPairingLink(linkID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.PairingLink{}, ErrPairingRefused
	}
	return link, err
}

// PairingNumber is the verification number of a redeemed link, derived on
// demand from the thumbprint its redemption recorded, and "" for a link
// nobody has redeemed.
func (s *Sessions) PairingNumber(link store.PairingLink) (string, error) {
	if !link.Redeemed() {
		return "", nil
	}
	return s.VerificationNumber(link.ID, link.KeyThumbprint)
}

// VerificationNumber derives the digits the owner compares between the two
// screens.
//
// Derived from the DEVICE KEY, keyed by this backend's signing secret and
// bound to the link id. Both properties are load-bearing:
//
//   - keying on the device key is what defeats the silent race. A link
//     redeemed by some other device produces a different number on the
//     minting surface than the one the owner's device is showing, so a
//     redemption the owner did not perform cannot be confirmed by
//     accident.
//   - binding to the link id means the same device pairing twice shows
//     different numbers, so a number read once is not a number that works
//     again.
//
// Only this backend can compute it, which is what lets both screens show a
// value they were each told rather than one they each derived — the device
// gets it in the redemption response, the minting surface from
// PairingStatus. Neither has to hold the other's material.
func (s *Sessions) VerificationNumber(linkID, thumbprint string) (string, error) {
	if linkID == "" || thumbprint == "" {
		return "", fmt.Errorf("identity: verification number needs a link and a key thumbprint")
	}
	key, err := s.EnsureSigningKey()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key.Secret.Reveal())
	mac.Write([]byte(verificationDomain))
	mac.Write([]byte(s.backendID))
	mac.Write([]byte{0})
	mac.Write([]byte(linkID))
	mac.Write([]byte{0})
	mac.Write([]byte(thumbprint))
	var sum [sha256.Size]byte
	mac.Sum(sum[:0])

	// Modulo a fixed power of ten over 64 uniform bits. The bias is
	// 2^64 mod 10^6 out of 2^64 — about one part in 10^13 — and stays
	// negligible for any digit count a person would read aloud, which is
	// why this does not need the rejection sampling the recovery-code
	// alphabet does (there the modulus is a set that could change size).
	value := binary.BigEndian.Uint64(sum[:8]) % pow10(verificationDigits)
	return fmt.Sprintf("%0*d", verificationDigits, value), nil
}

func pow10(digits int) uint64 {
	out := uint64(1)
	for range digits {
		out *= 10
	}
	return out
}

func (s *Sessions) auditPairingRefusal(reason Reason, peer, linkID string) {
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPairingRefused), Outcome: store.AuthAuditOutcomeRefused,
		Reason: reason.Code(), Peer: peer, Detail: linkID,
	})
}

// defaultDeviceLabel names a device that did not name itself. A row with an
// empty label is one nobody can identify in a device list, and the class is
// the most specific thing this backend knows without asking.
func defaultDeviceLabel(class DeviceClass) string {
	switch class {
	case DeviceBrowser:
		return "Browser"
	case DevicePhone:
		return "Phone"
	case DeviceCLI:
		return "Command line"
	case DeviceBackendPeer:
		return "Peer backend"
	case DeviceDesktop:
		fallthrough
	default:
		return "Device"
	}
}

// newPairingToken draws one link token and returns BOTH forms — the string
// the payload carries and the digest the row stores — for the same
// structural reason newRefreshSecret and newRecoveryCode do.
func newPairingToken() (token string, digest [sha256.Size]byte, err error) {
	buf := make([]byte, pairingTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", digest, fmt.Errorf("identity: draw pairing token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashPairingToken(token), nil
}

// hashPairingToken is what the store holds: domain-separated SHA-256 over
// 256 bits of CSPRNG output, on the same reasoning as hashRefreshSecret.
func hashPairingToken(token string) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(pairingHashDomain))
	h.Write([]byte(token))
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out
}

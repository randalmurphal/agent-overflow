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
	// step 6, §7).
	//
	// RESERVED. Phase 5 terminates TLS in-app and fills this; every link
	// minted before then carries an empty value, which is the
	// trust-on-first-use path the spec already describes for the
	// typed-code case — safe on proof-of-possession plus the verification
	// number, never on channel secrecy. It is declared now so the payload
	// shape does not move when phase 5 lands, and so a device built
	// against this version already knows the field exists.
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
	// UserID is the account the redeemed device binds to. Explicit, never
	// "the owner": a hub deployment mints links for many accounts (§11).
	UserID string
	// DeviceClass and BindingClass are decided HERE, by the minting
	// surface, and not by the redeeming device. A device that could name
	// its own binding class could name `loopback-only` and inherit the
	// local channel's posture.
	DeviceClass  DeviceClass
	BindingClass BindingClass
	// Scopes is the subset this link grants. A viewer link and a peer
	// invitation are this field, not a second flow.
	Scopes []Scope
	// CertFingerprint is the reserved phase-5 field (see PairingPayload).
	// Empty today on every path.
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
	if strings.TrimSpace(req.UserID) == "" {
		return PairingLink{}, fmt.Errorf("identity: mint pairing link: user id is required")
	}
	if !req.DeviceClass.Valid() {
		return PairingLink{}, fmt.Errorf("identity: %q is not a declared device class", string(req.DeviceClass))
	}
	if !req.BindingClass.Valid() {
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
	// KeyThumbprint is the device's public-key thumbprint, generated
	// before this call. REQUIRED on every redemption: proof-of-possession
	// is universal (§4 step 2), so there is no path where a link alone
	// admits a device.
	KeyThumbprint string
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
	if req.Token == "" || req.KeyThumbprint == "" {
		return Redemption{}, ReasonMissingProof
	}
	digest := hashPairingToken(req.Token)
	now := s.now().UnixMilli()

	// Spend the link FIRST. Everything after this point can still refuse
	// the redemption, and a refusal settles the link (cancelRedeemedLink)
	// rather than releasing it — a link that could be freed by a failed
	// redemption would be one a second presentation gets another turn at,
	// which is exactly what single-use forbids.
	pending, err := s.store.RedeemPairingLink(digest[:], now, req.KeyThumbprint)
	if errors.Is(err, sql.ErrNoRows) {
		s.auditPairingRefusal(ReasonUnknownCredential, req.Peer, "")
		return Redemption{}, ReasonUnknownCredential
	}
	if err != nil {
		log.Printf("identity: redeem pairing link: %v", err)
		return Redemption{}, ReasonUnknownCredential
	}

	device, reason := s.resolveRedeemingDevice(pending, req)
	if reason.Refused() {
		s.cancelRedeemedLink(pending.ID)
		s.auditPairingRefusal(reason, req.Peer, pending.ID)
		return Redemption{}, reason
	}

	session, tokens, err := s.mintPendingSession(pending, device, now)
	if err != nil {
		log.Printf("identity: mint pairing session for link %s: %v", pending.ID, err)
		s.cancelRedeemedLink(pending.ID)
		return Redemption{}, ReasonUnknownCredential
	}
	// The link must NAME what it produced before the device is told the
	// redemption worked: confirmation activates `link.session_id`, so a
	// link that reached this point without one is a pairing nothing could
	// ever complete. Fail it here, where the device learns to start over,
	// rather than at a confirmation the owner cannot explain.
	if err := s.store.AttachPairingRedemption(pending.ID, device.ID, session.ID); err != nil {
		log.Printf("identity: attach pairing redemption %s: %v", pending.ID, err)
		if _, revokeErr := s.RevokeSession(session.ID); revokeErr != nil {
			log.Printf("identity: revoke unattached pairing session %s: %v", session.ID, revokeErr)
		}
		s.cancelRedeemedLink(pending.ID)
		return Redemption{}, ReasonUnknownCredential
	}
	number, err := s.VerificationNumber(pending.ID, req.KeyThumbprint)
	if err != nil {
		log.Printf("identity: derive verification number for %s: %v", pending.ID, err)
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

// resolveRedeemingDevice finds or creates the device row for a presented
// key. See RedeemPairing for why an existing key is adopted and a revoked
// one is refused.
func (s *Sessions) resolveRedeemingDevice(link store.PairingLink, req RedemptionRequest) (store.Device, Reason) {
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = defaultDeviceLabel(DeviceClass(link.DeviceClass))
	}
	existing, err := s.store.DeviceByKeyThumbprint(req.KeyThumbprint)
	switch {
	case err == nil:
		if existing.RevokedAt != 0 {
			// The owner withdrew this device; a fresh link must not undo
			// that by itself. The reason names the remedy: restore the
			// device on the owner surface, then redeem a NEW link.
			return store.Device{}, ReasonRevokedDevice
		}
		if existing.UserID != link.UserID {
			return store.Device{}, ReasonKeyMismatch
		}
		if err := s.store.RelabelDevice(existing.ID, label, req.Platform); err != nil {
			log.Printf("identity: relabel device %s: %v", existing.ID, err)
		}
		existing.Label, existing.Platform = label, req.Platform
		return existing, ReasonNone
	case !errors.Is(err, sql.ErrNoRows):
		log.Printf("identity: read device by thumbprint: %v", err)
		return store.Device{}, ReasonKeyMismatch
	}
	device, err := s.store.CreatePairedDevice(
		link.UserID, label, link.DeviceClass, req.Platform, req.KeyThumbprint)
	if err != nil {
		log.Printf("identity: create paired device: %v", err)
		return store.Device{}, ReasonKeyMismatch
	}
	return device, ReasonNone
}

// mintPendingSession issues the unactivated session and its first
// credential pair. The window is the confirmation window, not the device
// class's access TTL: an unconfirmed credential must not outlive the
// decision it is waiting on. Confirm replaces it with the real one.
func (s *Sessions) mintPendingSession(link store.PairingLink, device store.Device, now int64) (store.Session, TokenSet, error) {
	scopes := make([]Scope, 0, len(link.Scopes))
	for _, name := range link.Scopes {
		scopes = append(scopes, Scope(name))
	}
	session, _, err := s.Mint(MintRequest{
		UserID:            link.UserID,
		DeviceID:          device.ID,
		BindingClass:      BindingClass(link.BindingClass),
		Scopes:            scopes,
		TTL:               PairingConfirmWindow,
		AwaitConfirmation: true,
	})
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	// Mint's own credential is discarded: it carries no refresh secret,
	// and issuing the pair through one path keeps a device from ever
	// holding an access credential whose renewal row does not exist.
	policy := PolicyFor(DeviceClass(device.Class), BindingClass(session.BindingClass))
	tokens, err := s.issueFor(session, policy, now)
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	return session, tokens, nil
}

// cancelRedeemedLink settles a link whose redemption could not complete, so
// the spent link cannot be presented again by anything.
func (s *Sessions) cancelRedeemedLink(linkID string) {
	if _, err := s.store.CancelPairingLink(linkID, s.now().UnixMilli()); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		log.Printf("identity: cancel pairing link %s: %v", linkID, err)
	}
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
	now := s.now().UnixMilli()
	link, err := s.store.ConfirmPairingLink(linkID, now)
	if errors.Is(err, sql.ErrNoRows) {
		return store.PairingLink{}, ErrPairingRefused
	}
	if err != nil {
		return store.PairingLink{}, err
	}
	device, err := s.store.GetDevice(link.DeviceID)
	if err != nil {
		return store.PairingLink{}, fmt.Errorf("identity: confirm pairing: read device: %w", err)
	}
	policy := PolicyFor(DeviceClass(device.Class), BindingClass(link.BindingClass))
	moved, err := s.store.ActivateSession(link.SessionID, now, now+policy.Access.Milliseconds())
	if err != nil {
		return store.PairingLink{}, err
	}
	if !moved {
		// The session was revoked or expired between redemption and
		// confirmation. The link is settled either way; say so rather
		// than reporting a pairing that admits nothing as complete.
		return store.PairingLink{}, ErrPairingRefused
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPairingConfirmed), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: link.UserID, DeviceID: link.DeviceID, SessionID: link.SessionID,
		Detail: link.ID,
	})
	return link, nil
}

// CancelPairing refuses a link — the verification number did not match, or
// it was minted by mistake — and revokes whatever a redemption already
// created.
//
// Revoking after cancelling, in that order, for the same reason
// RevokeSession orders its own three steps: the link must stop admitting
// anything before the session it created is torn down, or a redemption in
// flight could mint a second one behind the teardown.
func (s *Sessions) CancelPairing(linkID string) (store.PairingLink, error) {
	link, err := s.store.CancelPairingLink(linkID, s.now().UnixMilli())
	if errors.Is(err, sql.ErrNoRows) {
		return store.PairingLink{}, ErrPairingRefused
	}
	if err != nil {
		return store.PairingLink{}, err
	}
	if link.SessionID != "" {
		if _, err := s.RevokeSession(link.SessionID); err != nil {
			return store.PairingLink{}, err
		}
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPairingCanceled), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: link.UserID, DeviceID: link.DeviceID, SessionID: link.SessionID,
		Detail: link.ID,
	})
	return link, nil
}

// PairingStatus reads one link, for the minting surface to poll while it
// waits for a device to redeem. It carries no secret: the token is not
// stored, and the verification number is derived on demand from the
// thumbprint the redemption recorded.
func (s *Sessions) PairingStatus(linkID string) (store.PairingLink, string, error) {
	link, err := s.store.GetPairingLink(linkID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.PairingLink{}, "", ErrPairingRefused
	}
	if err != nil {
		return store.PairingLink{}, "", err
	}
	if !link.Redeemed() {
		return link, "", nil
	}
	number, err := s.VerificationNumber(link.ID, link.KeyThumbprint)
	if err != nil {
		return store.PairingLink{}, "", err
	}
	return link, number, nil
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
	mac := hmac.New(sha256.New, key.Secret)
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

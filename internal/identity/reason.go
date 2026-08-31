package identity

// Reason is why a presentation was refused. A closed typed set, not a
// string: a refusal travels from the verifier through the audit log to the
// wire and into a client-side hint, and every hop has to agree on the same
// finite vocabulary. A free-form string would let one hop invent a value
// the next cannot present.
//
// The set is exactly what verification and revocation can produce. It is
// not a catalogue of everything that could ever go wrong — a reason no
// code path emits is a case the client-side presentation would carry
// forever without ever showing it. `reason_gate_test.go` fails on a
// constant with no code, a code with no constant, and a non-contiguous
// ordinal.
//
// ReasonNone is the zero value and means "not refused". A caller that
// checks `if reason != ReasonNone` therefore gets the right answer from an
// unset variable, which is the failure mode worth designing for.
type Reason uint8

const (
	// ReasonNone means the presentation was admitted.
	ReasonNone Reason = iota
	// ReasonMissingProof means nothing was presented at all.
	ReasonMissingProof
	// ReasonMalformedProof means the credential did not parse: wrong
	// prefix, wrong field count, wrong lengths, or bad base64. Nothing
	// about the claimed session was learned, because nothing was readable.
	ReasonMalformedProof
	// ReasonKeyMismatch means the claims name a signing key this backend
	// does not hold. Distinct from an invalid signature: the credential is
	// well-formed and may be perfectly valid somewhere else — a database
	// restored under a new backend identity, or a key deliberately
	// dropped. The remedy is to re-authenticate, not to suspect the proof.
	ReasonKeyMismatch
	// ReasonInvalidSignature means the MAC did not verify under the named
	// key. Checked BEFORE any time window, so a proof that does not verify
	// is never reported as a clock problem.
	ReasonInvalidSignature
	// ReasonOutsideTimeWindow means a verified proof claims to have been
	// issued in the future by more than the accepted skew. Today that is
	// this host's own clock moving backwards (a suspended VM, a laptop
	// that reaches NTP after boot); from phase 5 it is also where a
	// client-minted proof's timestamp lands. Its hint is the date-and-time
	// one, which is why it must never be reachable from an unverified
	// proof.
	ReasonOutsideTimeWindow
	// ReasonUnknownSession means the signature verified but no session row
	// carries that id. The row is the authoritative half; a valid signature
	// over a session that does not exist admits nothing.
	ReasonUnknownSession
	// ReasonRevokedSession means the session row exists and was revoked.
	ReasonRevokedSession
	// ReasonExpiredSession means the session's validity window has passed —
	// either the window signed into the claims or the row's own expiry,
	// which can be shortened after the fact. Both have the same remedy.
	ReasonExpiredSession
	// ReasonPendingConfirmation means the session exists and is inside its
	// window, but the owner has not yet confirmed the pairing verification
	// number on the device that minted the link (§4 "Pairing"). The only
	// refusal in this set whose remedy is on ANOTHER device, and the only
	// one where presenting the same credential again is expected to
	// succeed shortly — which is why it is not folded into
	// ReasonUnknownSession.
	ReasonPendingConfirmation
	// ReasonUnknownCredential means the presented pairing token or refresh
	// secret names nothing this backend holds, or nothing it will still
	// honour. Distinct from ReasonUnknownSession, which is about a session
	// id inside verified claims: this one is about a bare secret, where a
	// spent value, an expired value, and a value that never existed all
	// answer the same thing on purpose.
	ReasonUnknownCredential
)

// reasonCodes maps each Reason to its stable wire spelling, indexed by
// ordinal. Contiguous ordinals are what make this a slice instead of a
// map, and the gate test is what keeps them contiguous.
//
// A code is stable forever once shipped: an older client bundle may still
// be mapping it to a hint.
var reasonCodes = [...]string{
	ReasonNone:              "",
	ReasonMissingProof:      "missing_proof",
	ReasonMalformedProof:    "malformed_proof",
	ReasonKeyMismatch:       "key_mismatch",
	ReasonInvalidSignature:  "invalid_signature",
	ReasonOutsideTimeWindow: "outside_time_window",
	ReasonUnknownSession:    "unknown_session",
	ReasonRevokedSession:    "revoked_session",
	ReasonExpiredSession:      "expired_session",
	ReasonPendingConfirmation: "pending_confirmation",
	ReasonUnknownCredential:   "unknown_credential",
}

// Code returns the stable wire spelling. An out-of-range value — only
// reachable by constructing a Reason from an integer — answers
// "malformed_proof" rather than an empty string, so a corrupt value can
// never read as "admitted".
func (r Reason) Code() string {
	if int(r) >= len(reasonCodes) {
		return reasonCodes[ReasonMalformedProof]
	}
	return reasonCodes[r]
}

// String makes Reason printable in logs and test failures.
func (r Reason) String() string {
	if r == ReasonNone {
		return "none"
	}
	return r.Code()
}

// Refused reports whether this reason denies a presentation.
func (r Reason) Refused() bool { return r != ReasonNone }

// ReasonFromCode resolves a wire spelling back to its constant. The false
// answer is for a code from a newer backend than this build knows.
func ReasonFromCode(code string) (Reason, bool) {
	if code == "" {
		return ReasonNone, true
	}
	for ordinal, candidate := range reasonCodes {
		if candidate == code {
			return Reason(ordinal), true
		}
	}
	return ReasonMalformedProof, false
}

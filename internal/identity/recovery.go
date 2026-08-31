package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// Recovery codes cover the case every other credential path cannot: a new
// phone, a dead laptop, nobody at the host (docs/specs/remote-access.md
// §3). They are minted with the owner account, shown once, and spent once.

const (
	// RecoveryCodeCount is how many codes a mint produces. Ten is enough
	// that losing a printed sheet's top half still leaves a way in, and few
	// enough that someone will actually store them.
	RecoveryCodeCount = 10

	// recoveryCodeGroups × recoveryCodeGroupLen characters of the alphabet
	// below carry 100 bits — far past any offline search of the stored
	// hashes, which is what lets those hashes be a plain SHA-256 keyed by
	// nothing. A slow KDF would buy nothing here (the input is not
	// human-chosen) and would cost the single indexed lookup that makes
	// consumption atomic.
	recoveryCodeGroups   = 4
	recoveryCodeGroupLen = 5

	// recoveryAlphabet is Crockford base32 minus its ambiguous letters, so
	// a code read aloud or typed from paper has no I/L/O/U to guess at.
	recoveryAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// ErrRecoveryCodeRefused is returned for a code that matches no unspent
// row. A replay of a spent code and a code that never existed both land
// here, because the backend genuinely cannot tell them apart — and neither
// can a caller, which is the point.
var ErrRecoveryCodeRefused = errors.New("identity: recovery code refused")

// MintRecoveryCodes replaces one account's unspent codes with a fresh set
// and returns the plaintext exactly once. Nothing stores the returned
// strings; a caller that loses them has to mint again.
func (s *Sessions) MintRecoveryCodes(userID string) ([]string, error) {
	if userID == "" {
		return nil, fmt.Errorf("identity: mint recovery codes: user id is required")
	}
	codes := make([]string, 0, RecoveryCodeCount)
	hashes := make([][]byte, 0, RecoveryCodeCount)
	for range RecoveryCodeCount {
		display, normalized, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, display)
		hash := hashRecoveryCode(normalized)
		hashes = append(hashes, hash[:])
	}
	if err := s.store.ReplaceRecoveryCodes(userID, hashes, s.now().UnixMilli()); err != nil {
		return nil, err
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditRecoveryCodesMinted), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: userID, Detail: fmt.Sprintf("%d codes", len(codes)),
	})
	return codes, nil
}

// ConsumeRecoveryCode spends a code and returns the account it admits.
// Single use is decided by the store's one-statement update, so several
// callers presenting the same code produce exactly one winner.
//
// byDevice is recorded on the spent row and may be empty when the device
// does not exist yet — which is the ordinary case, since a recovery code
// is what a device without one presents.
func (s *Sessions) ConsumeRecoveryCode(code, byDevice, peer string) (string, error) {
	normalized := NormalizeRecoveryCode(code)
	if normalized == "" {
		s.auditRecoveryRefusal(byDevice, peer)
		return "", ErrRecoveryCodeRefused
	}
	hash := hashRecoveryCode(normalized)
	userID, err := s.store.ConsumeRecoveryCode(hash[:], s.now().UnixMilli(), byDevice)
	if errors.Is(err, sql.ErrNoRows) {
		s.auditRecoveryRefusal(byDevice, peer)
		return "", ErrRecoveryCodeRefused
	}
	if err != nil {
		return "", err
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditRecoveryCodeConsumed), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: userID, DeviceID: byDevice, Peer: peer,
	})
	return userID, nil
}

func (s *Sessions) auditRecoveryRefusal(byDevice, peer string) {
	s.audit(store.AuthAuditEntry{
		Event: string(AuditRecoveryCodeRefused), Outcome: store.AuthAuditOutcomeRefused,
		DeviceID: byDevice, Peer: peer,
	})
}

// NormalizeRecoveryCode folds a typed code to its canonical form: upper
// case, separators removed. Someone reading a code off paper types the
// dashes, or does not, or holds shift — all three have to reach the same
// hash, or the code would work only when entered exactly as displayed.
//
// Returns "" for anything carrying a character outside the alphabet, which
// the caller treats as a refusal rather than hashing a value that cannot
// match.
func NormalizeRecoveryCode(code string) string {
	var out strings.Builder
	out.Grow(recoveryCodeGroups * recoveryCodeGroupLen)
	for _, r := range strings.ToUpper(strings.TrimSpace(code)) {
		switch r {
		case '-', ' ':
			continue
		}
		if !strings.ContainsRune(recoveryAlphabet, r) {
			return ""
		}
		out.WriteRune(r)
	}
	if out.Len() != recoveryCodeGroups*recoveryCodeGroupLen {
		return ""
	}
	return out.String()
}

// newRecoveryCode draws one code and returns BOTH forms: the dashed one a
// person reads, and the normalized one that gets hashed.
//
// Returning both is the structural half of the rule that only the
// normalized form is ever hashed. A single return value would let a caller
// hash the dashed string, producing codes that verify against nothing —
// and the mint path has no way to notice, because it never tries them.
//
// Rejection sampling on the random byte keeps every character equally
// likely. Taking a raw byte modulo the alphabet length happens to be
// uniform at 32, and would stop being uniform, silently, the day the
// alphabet changed length.
func newRecoveryCode() (display, normalized string, err error) {
	const limit = 256 - (256 % len(recoveryAlphabet))
	var shown, plain strings.Builder
	shown.Grow(recoveryCodeGroups*recoveryCodeGroupLen + recoveryCodeGroups - 1)
	plain.Grow(recoveryCodeGroups * recoveryCodeGroupLen)
	buf := make([]byte, 1)
	for group := range recoveryCodeGroups {
		if group > 0 {
			shown.WriteByte('-')
		}
		for range recoveryCodeGroupLen {
			for {
				if _, err := rand.Read(buf); err != nil {
					return "", "", fmt.Errorf("identity: draw recovery code: %w", err)
				}
				if int(buf[0]) < limit {
					c := recoveryAlphabet[int(buf[0])%len(recoveryAlphabet)]
					shown.WriteByte(c)
					plain.WriteByte(c)
					break
				}
			}
		}
	}
	return shown.String(), plain.String(), nil
}

// hashRecoveryCode is what the store holds. The normalized code only —
// never the formatted one — so the dashes a person sees are presentation
// and nothing else.
func hashRecoveryCode(normalized string) [sha256.Size]byte {
	return sha256.Sum256([]byte(normalized))
}

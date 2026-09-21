package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"runtime"

	"agent-overflow/internal/store"
	"github.com/google/uuid"
)

const LocalChannel = "local"
const localChannelLabel = "This computer"
const localCredentialPrefix = "aol1."

// EnsureLocalChannelSession resolves durable attribution and issues one opaque
// credential per session service. The secret is never persisted, so restart
// invalidates it without a shutdown write or a wall-clock deadline.
func (s *Sessions) EnsureLocalChannelSession(userID string) (store.Session, TokenSet, error) {
	s.localMu.Lock()
	defer s.localMu.Unlock()
	if userID == "" {
		return store.Session{}, TokenSet{}, fmt.Errorf("identity: local channel needs a user id")
	}
	device, err := s.store.EnsureChannelDevice(userID, LocalChannel, localChannelLabel, string(DeviceDesktop), runtime.GOOS)
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	if device.RevokedAt != 0 {
		return store.Session{}, TokenSet{}, store.ErrDeviceRevoked
	}
	key, err := s.EnsureSigningKey()
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	scopes, err := ValidateScopes(Scopes)
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	session, err := s.store.EnsureLocalSession(store.Session{
		ID: uuid.NewString(), UserID: userID, DeviceID: device.ID,
		BindingClass: string(BindingLoopbackOnly), Scopes: scopes,
		SigningKeyID: key.ID, CreatedAt: s.Now(), ActivatedAt: s.Now(),
	})
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	// Always consult admission, including repeated initialization after revocation.
	s.forget(session.ID)
	session, reason := s.Live(session.ID)
	if reason.Refused() {
		return store.Session{}, TokenSet{}, fmt.Errorf("identity: local channel: %s", reason.Code())
	}
	if s.localCredential == "" {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return store.Session{}, TokenSet{}, fmt.Errorf("identity: local credential: %w", err)
		}
		s.localCredential = localCredentialPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
		s.localSessionID = session.ID
	}
	if s.localSessionID != session.ID {
		return store.Session{}, TokenSet{}, fmt.Errorf("identity: local session changed during this boot")
	}
	return session, TokenSet{SessionID: session.ID, Credential: s.localCredential, Scopes: session.Scopes}, nil
}

func (s *Sessions) verifyLocal(credential string) (store.Session, Reason) {
	s.localMu.Lock()
	matches := s.localCredential != "" && subtle.ConstantTimeCompare([]byte(credential), []byte(s.localCredential)) == 1
	sessionID := s.localSessionID
	s.localMu.Unlock()
	if !matches {
		return store.Session{}, ReasonInvalidSignature
	}
	return s.Live(sessionID)
}

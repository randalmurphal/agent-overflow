package storetest

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// PairedDevice writes a new device of class for userID together with one
// live session holding scopes, through the transaction production writes a
// signed-in device with (store.CreateDeviceSession).
func PairedDevice(tb testing.TB, s *store.Store, userID, label, class string, scopes ...string) (store.Device, store.Session) {
	tb.Helper()
	device, err := store.NewPairedDevice(userID, label, class, "linux", "thumb-"+uuid.NewString(), "bearer")
	if err != nil {
		tb.Fatalf("storetest: build device: %v", err)
	}
	session := liveSession(tb, s, device, scopes, time.Hour)
	if err := s.CreateDeviceSession(store.DeviceSession{Device: device, NewDevice: true, Session: session}); err != nil {
		tb.Fatalf("storetest: write device %s with its session: %v", label, err)
	}
	return device, session
}

// LiveSession writes another live session holding scopes on an existing
// device, through the same transaction.
func LiveSession(tb testing.TB, s *store.Store, device store.Device, scopes []string, ttl time.Duration) store.Session {
	tb.Helper()
	session := liveSession(tb, s, device, scopes, ttl)
	if err := s.CreateDeviceSession(store.DeviceSession{Device: device, Session: session}); err != nil {
		tb.Fatalf("storetest: write a session on device %s: %v", device.ID, err)
	}
	return session
}

func liveSession(tb testing.TB, s *store.Store, device store.Device, scopes []string, ttl time.Duration) store.Session {
	tb.Helper()
	now := time.Now().UnixMilli()
	return store.Session{
		ID: uuid.NewString(), UserID: device.UserID, DeviceID: device.ID, BindingClass: "device-bound",
		Scopes: scopes, SigningKeyID: signingKey(tb, s).ID, CreatedAt: now, ExpiresAt: now + ttl.Milliseconds(),
		ActivatedAt: now,
	}
}

// signingKey is the store's active signing key, written first when the store
// holds none yet.
func signingKey(tb testing.TB, s *store.Store) store.SigningKey {
	tb.Helper()
	key, err := s.ActiveSigningKey()
	if err == nil {
		return key
	}
	if !errors.Is(err, sql.ErrNoRows) {
		tb.Fatalf("storetest: read the signing key: %v", err)
	}
	key = store.SigningKey{ID: "0123456789abcdef", Secret: secret.New([]byte("storetest signing key bytes....")), CreatedAt: 1}
	if err := s.InsertSigningKey(key); err != nil {
		tb.Fatalf("storetest: write a signing key: %v", err)
	}
	return key
}

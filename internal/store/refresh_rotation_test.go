package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
)

func refreshRotationFixture(t *testing.T) (*Store, RefreshRotation) {
	t.Helper()
	s, session := seedRefreshSession(t)
	in := RefreshRotation{SessionID: session.ID, DeviceID: session.DeviceID,
		OldHash: bytes.Repeat([]byte{1}, 32), NextHash: bytes.Repeat([]byte{2}, 32),
		Now: 2000, AccessUntil: 1000000, RefreshUntil: 2000000, Recoverable: true}
	if _, err := s.CreateRefreshSecret(session.ID, in.OldHash, 1000, 900000); err != nil {
		t.Fatal(err)
	}
	return s, in
}

func TestRefreshRotationRecoversTheSameCommittedOperation(t *testing.T) {
	s, in := refreshRotationFixture(t)
	first, err := s.RotateRefreshSecret(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Session.ExpiresAt != in.AccessUntil {
		t.Fatalf("first: %+v", first)
	}
	// The reply was lost. The same persisted successor recovers the result,
	// including after the short-lived access credential has expired.
	in.Now = in.AccessUntil + 1
	in.AccessUntil = in.Now + 10000
	again, err := s.RotateRefreshSecret(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Secret.ID != first.Secret.ID || again.Session.ExpiresAt != in.AccessUntil {
		t.Fatalf("retry: %+v", again)
	}
	secrets, err := s.ListRefreshSecretsForSession(in.SessionID)
	if err != nil || len(secrets) != 2 {
		t.Fatalf("retry created a generation: %d, %v", len(secrets), err)
	}
	if _, err := s.DeleteRefreshSecretsExpiredBefore(in.Now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRefreshSecretByHash(in.OldHash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("predecessor not pruned: %v", err)
	}
	if recovered, err := s.RotateRefreshSecret(context.Background(), in); err != nil || !recovered.Replayed {
		t.Fatalf("pruned predecessor: %+v, %v", recovered, err)
	}
}

func TestRefreshRotationDistinguishesReuseFromASupersededReceipt(t *testing.T) {
	s, in := refreshRotationFixture(t)
	if _, err := s.RotateRefreshSecret(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	different := in
	different.NextHash = bytes.Repeat([]byte{3}, 32)
	if _, err := s.RotateRefreshSecret(context.Background(), different); !errors.Is(err, ErrRefreshReuse) {
		t.Fatalf("different request: %v", err)
	}
	next := different
	next.OldHash = in.NextHash
	if _, err := s.RotateRefreshSecret(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateRefreshSecret(context.Background(), in); !errors.Is(err, ErrRefreshSuperseded) {
		t.Fatalf("old receipt: %v", err)
	}
}

func TestRefreshRotationFailureDoesNotSpendItsPredecessor(t *testing.T) {
	s, in := refreshRotationFixture(t)
	if _, err := s.CreateRefreshSecret(in.SessionID, in.NextHash, 1000, 2000000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateRefreshSecret(context.Background(), in); err == nil {
		t.Fatal("successor collision admitted")
	}
	old, err := s.GetRefreshSecretByHash(in.OldHash)
	if err != nil || old.Spent() {
		t.Fatalf("failed transaction spent predecessor: %+v, %v", old, err)
	}
	session, err := s.GetSession(in.SessionID)
	if err != nil || session.ExpiresAt == in.AccessUntil {
		t.Fatalf("failed transaction extended session: %+v, %v", session, err)
	}
}

func TestRefreshRotationRechecksRevocationInsideTransaction(t *testing.T) {
	s, in := refreshRotationFixture(t)
	if _, err := s.db.Exec(`UPDATE devices SET revoked_at = ? WHERE id = ?`, in.Now, in.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateRefreshSecret(context.Background(), in); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked device: %v", err)
	}
	old, err := s.GetRefreshSecretByHash(in.OldHash)
	if err != nil || old.Spent() {
		t.Fatalf("revoked device spent predecessor: %+v, %v", old, err)
	}
}

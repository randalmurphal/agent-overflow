//go:build !noremote

package deviceclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// redeemed spends a fresh link against a backend that answers at once.
func redeemed(t *testing.T, dir string) *Redemption {
	t.Helper()
	h := newHeldRedemption(t)
	close(h.release)
	redemption, err := Redeem(context.Background(), dir, h.link, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redemption.client.http.CloseIdleConnections)
	return redemption
}

// The save decides on the session stored now, and a refusal is its answer
// with nothing written.
func TestASaveAdmitsOnTheStoredSessionAndARefusalWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	redemption := redeemed(t, dir)
	stored := Session{BackendID: "backend-a", Endpoint: "https://other.example", SessionID: "other-manager", Credential: "ao1.other"}
	if err := SaveSession(dir, stored); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	var seen Session
	var seenErr error
	if _, _, err := redemption.Save(func(s Session, err error) error { seen, seenErr = s, err; return refused }); !errors.Is(err, refused) {
		t.Fatalf("Save after a refusal = %v; want the refusal", err)
	}
	if seenErr != nil || seen.SessionID != "other-manager" {
		t.Fatalf("admit saw %+v, %v; want the stored session", seen, seenErr)
	}
	if kept, err := LoadSession(dir, "backend-a"); err != nil || kept.SessionID != "other-manager" {
		t.Fatalf("stored after a refusal = %+v, %v; want it unchanged", kept, err)
	}
}

// A redemption saves once. Every later call writes nothing, whatever
// happened to the stored session since.
func TestASaveIsSingleUse(t *testing.T) {
	t.Parallel()
	accept := func(Session, error) error { return nil }
	t.Run("twice", func(t *testing.T) {
		t.Parallel()
		redemption := redeemed(t, t.TempDir())
		if _, _, err := redemption.Save(accept); err != nil {
			t.Fatal(err)
		}
		if _, _, err := redemption.Save(accept); !errors.Is(err, errRedemptionSpent) {
			t.Fatalf("second Save = %v; want errRedemptionSpent", err)
		}
	})
	t.Run("concurrently", func(t *testing.T) {
		t.Parallel()
		redemption := redeemed(t, t.TempDir())
		var writes atomic.Int32
		admit := func(Session, error) error { writes.Add(1); return nil }
		errs := make(chan error, 2)
		for range 2 {
			go func() { _, _, err := redemption.Save(admit); errs <- err }()
		}
		first, second := <-errs, <-errs
		if (first == nil) == (second == nil) || !errors.Is(errors.Join(first, second), errRedemptionSpent) {
			t.Fatalf("concurrent Saves = %v, %v; want one saved and one errRedemptionSpent", first, second)
		}
		if n := writes.Load(); n != 1 {
			t.Fatalf("%d saves reached the stored session; want 1", n)
		}
	})
	t.Run("after a renewal", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		redemption := redeemed(t, dir)
		client, _, err := redemption.Save(accept)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Renew(t.Context()); err != nil {
			t.Fatal(err)
		}
		renewed, err := LoadSession(dir, "backend-a")
		if err != nil || renewed.RefreshSecret == redemption.session.RefreshSecret {
			t.Fatalf("after Renew: %+v, %v; want a rotated session", renewed, err)
		}
		if _, _, err := redemption.Save(accept); !errors.Is(err, errRedemptionSpent) {
			t.Fatalf("Save after a renewal = %v; want errRedemptionSpent", err)
		}
		if kept, err := LoadSession(dir, "backend-a"); err != nil || kept.RefreshSecret != renewed.RefreshSecret {
			t.Fatalf("stored after the second Save = %+v, %v; want the renewed session", kept, err)
		}
	})
	t.Run("after a forget", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		redemption := redeemed(t, dir)
		client, _, err := redemption.Save(accept)
		if err != nil {
			t.Fatal(err)
		}
		client.Retire()
		if err := ForgetSession(dir, "backend-a"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := redemption.Save(accept); !errors.Is(err, errRedemptionSpent) {
			t.Fatalf("Save after a forget = %v; want errRedemptionSpent", err)
		}
		if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
			t.Fatalf("stored after the second Save = %v; want nothing", err)
		}
	})
}

// A conditional forget decides on the session stored now, and leaves what
// it refuses, what is absent and what cannot be read.
func TestForgetSessionIfDecidesOnTheStoredSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if dropped, err := ForgetSessionIf(dir, "backend-a", func(Session) bool { return true }); dropped || err != nil {
		t.Fatalf("with nothing stored = %v, %v; want false, nil", dropped, err)
	}
	stored := Session{BackendID: "backend-a", Endpoint: "https://a.example", SessionID: "newer", Credential: "ao1.newer"}
	if err := SaveSession(dir, stored); err != nil {
		t.Fatal(err)
	}
	named := func(id string) func(Session) bool { return func(s Session) bool { return s.SessionID == id } }
	if dropped, err := ForgetSessionIf(dir, "backend-a", named("older")); dropped || err != nil {
		t.Fatalf("for another session = %v, %v; want false, nil", dropped, err)
	}
	if _, err := LoadSession(dir, "backend-a"); err != nil {
		t.Fatalf("a refused forget dropped the session: %v", err)
	}
	if dropped, err := ForgetSessionIf(dir, "backend-a", named("newer")); !dropped || err != nil {
		t.Fatalf("for the stored session = %v, %v; want true, nil", dropped, err)
	}
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after the forget = %v; want nothing stored", err)
	}
	path := filepath.Join(dir, SessionsDirName, "backend-a.json")
	if err := os.WriteFile(path, []byte(`{"backendId":"backend-a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if dropped, err := ForgetSessionIf(dir, "backend-a", func(Session) bool { return true }); dropped || err == nil {
		t.Fatalf("over an unreadable file = %v, %v; want false and the read error", dropped, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the unreadable file was dropped: %v", err)
	}
}

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

// saveHeld saves a redemption under its session file's lock, as an owner
// that decided to keep it does.
func saveHeld(dir string, r *Redemption, nickname string) (*Client, Pairing, error) {
	var client *Client
	var pairing Pairing
	err := WithSession(dir, r.session.BackendID, func(held *HeldSession) (err error) {
		client, pairing, err = r.Save(held, nickname)
		return err
	})
	return client, pairing, err
}

// A held session answers what is stored now, follows its own writes, and
// is unusable once the lock is released.
func TestAHeldSessionReadsAndWritesUnderItsLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stored := Session{BackendID: "backend-a", Endpoint: "https://a.example", SessionID: "stored", Credential: "ao1.stored"}
	if err := SaveSession(dir, stored); err != nil {
		t.Fatal(err)
	}
	var kept *HeldSession
	err := WithSession(dir, "backend-a", func(held *HeldSession) error {
		kept = held
		if got, err := held.Stored(); err != nil || got.SessionID != "stored" {
			t.Fatalf("Stored = %+v, %v; want the stored session", got, err)
		}
		other := stored
		other.BackendID = "backend-b"
		if err := held.Write(other); err == nil {
			t.Fatal("wrote another backend's session under this one's file")
		}
		if err := held.Remove(); err != nil {
			t.Fatal(err)
		}
		if _, err := held.Stored(); !errors.Is(err, ErrNoSession) {
			t.Fatalf("Stored after Remove = %v; want ErrNoSession", err)
		}
		replacement := stored
		replacement.SessionID = "replacement"
		if err := held.Write(replacement); err != nil {
			t.Fatal(err)
		}
		if got, err := held.Stored(); err != nil || got.SessionID != "replacement" {
			t.Fatalf("Stored after Write = %+v, %v; want the replacement", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := LoadSession(dir, "backend-a"); err != nil || got.SessionID != "replacement" {
		t.Fatalf("stored after the hold = %+v, %v; want the replacement", got, err)
	}
	if err := kept.Write(stored); !errors.Is(err, errSessionReleased) {
		t.Fatalf("Write after the hold = %v; want errSessionReleased", err)
	}
	if err := kept.Remove(); !errors.Is(err, errSessionReleased) {
		t.Fatalf("Remove after the hold = %v; want errSessionReleased", err)
	}
	if _, err := kept.Stored(); !errors.Is(err, errSessionReleased) {
		t.Fatalf("Stored after the hold = %v; want errSessionReleased", err)
	}
	path := filepath.Join(dir, SessionsDirName, "backend-a.json")
	if err := os.WriteFile(path, []byte(`{"backendId":"backend-a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ForgetSession(dir, "backend-a"); err != nil {
		t.Fatalf("ForgetSession over an unreadable file: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the unreadable file after ForgetSession: %v; want it gone", err)
	}
}

// A save names the replacement as its caller says.
func TestASaveKeepsTheNicknameItIsGiven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	client, _, err := saveHeld(dir, redeemed(t, dir), "GPU computer")
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := LoadSession(dir, "backend-a"); err != nil || saved.Nickname != "GPU computer" || client.Session().Nickname != "GPU computer" {
		t.Fatalf("saved %+v, %v; want the nickname given", saved, err)
	}
}

// A redemption saves once. Every later call writes nothing, whatever
// happened to the stored session since.
func TestASaveIsSingleUse(t *testing.T) {
	t.Parallel()
	t.Run("twice", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		redemption := redeemed(t, dir)
		if _, _, err := saveHeld(dir, redemption, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := saveHeld(dir, redemption, ""); !errors.Is(err, errRedemptionSpent) {
			t.Fatalf("second Save = %v; want errRedemptionSpent", err)
		}
	})
	t.Run("concurrently", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		redemption := redeemed(t, dir)
		var saved atomic.Int32
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				_, _, err := saveHeld(dir, redemption, "")
				if err == nil {
					saved.Add(1)
				}
				errs <- err
			}()
		}
		first, second := <-errs, <-errs
		if saved.Load() != 1 || !errors.Is(errors.Join(first, second), errRedemptionSpent) {
			t.Fatalf("concurrent Saves = %v, %v; want one saved and one errRedemptionSpent", first, second)
		}
	})
	t.Run("after a renewal", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		redemption := redeemed(t, dir)
		client, _, err := saveHeld(dir, redemption, "")
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
		if _, _, err := saveHeld(dir, redemption, ""); !errors.Is(err, errRedemptionSpent) {
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
		client, _, err := saveHeld(dir, redemption, "")
		if err != nil {
			t.Fatal(err)
		}
		client.Retire()
		if err := ForgetSession(dir, "backend-a"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := saveHeld(dir, redemption, ""); !errors.Is(err, errRedemptionSpent) {
			t.Fatalf("Save after a forget = %v; want errRedemptionSpent", err)
		}
		if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
			t.Fatalf("stored after the second Save = %v; want nothing", err)
		}
	})
}

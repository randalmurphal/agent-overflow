//go:build !noremote

package deviceclient

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A redemption is saved only in the profile directory whose device key it
// was redeemed with; a held session of another directory is refused, writes
// nothing and leaves the redemption unspent.
func TestASaveRefusesAnotherProfilesHeldSession(t *testing.T) {
	t.Parallel()
	dir, other := t.TempDir(), t.TempDir()
	r := redeemed(t, dir)
	if _, _, err := saveHeld(other, r, ""); err == nil {
		t.Fatal("saved a redemption into another profile directory")
	}
	if _, err := LoadSession(other, r.session.BackendID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("the other directory after the refused save = %v; want nothing stored", err)
	}
	if _, _, err := saveHeld(dir, r, ""); err != nil {
		t.Fatalf("save into its own directory after the refusal: %v", err)
	}
}

// A copy of a held session shares its state, so it cannot write once the
// lock is released.
func TestACopyOfAHeldSessionCannotWriteAfterRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var copied HeldSession
	if err := WithSession(dir, "backend-a", func(held *HeldSession) error {
		copied = *held
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session := Session{BackendID: "backend-a", Endpoint: "https://a.example", SessionID: "s", Credential: "c"}
	if err := copied.Write(session); !errors.Is(err, errSessionReleased) {
		t.Fatalf("a copy's write after the release = %v; want %v", err, errSessionReleased)
	}
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("stored after the copy's write = %v; want nothing", err)
	}
}

// Restore puts the session file back as the lock found it, byte for byte or
// absent, also after a write that took effect and answered an error.
func TestARestorePutsTheFileBackAsFound(t *testing.T) {
	t.Parallel()
	replacement := Session{BackendID: "backend-a", Endpoint: "https://b.example", SessionID: "new", Credential: "c2"}
	for _, start := range []string{"stored", "absent", "undecodable", "write applied with an error"} {
		t.Run(start, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, SessionsDirName, "backend-a.json")
			var before []byte
			switch start {
			case "stored", "write applied with an error":
				if err := SaveSession(dir, Session{BackendID: "backend-a", Endpoint: "https://a.example", SessionID: "old", Credential: "c1"}); err != nil {
					t.Fatal(err)
				}
			case "undecodable":
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ = os.ReadFile(path)
			if start == "write applied with an error" {
				// The rename into a write-only directory succeeds and the
				// directory sync, which must open it, fails.
				sessions := filepath.Dir(path)
				if err := os.Chmod(sessions, 0o300); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(sessions, 0o700) })
			}
			err := WithSession(dir, "backend-a", func(held *HeldSession) error {
				writeErr := held.Write(replacement)
				if start == "write applied with an error" {
					if writeErr == nil {
						t.Fatal("the write into a write-only directory answered no error")
					}
					if got, err := held.Stored(); err != nil || got.SessionID != "new" {
						t.Fatalf("Stored after the applied write = %+v, %v; want the replacement on disk", got, err)
					}
				} else if writeErr != nil {
					t.Fatal(writeErr)
				}
				restored, err := held.Restore()
				if !restored {
					t.Fatalf("Restore = not restored, %v", err)
				}
				if (err != nil) != (start == "write applied with an error") {
					t.Fatalf("Restore error = %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			after, readErr := os.ReadFile(path)
			if start == "absent" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("after Restore the file = %q, %v; want none", after, readErr)
				}
				return
			}
			if readErr != nil || !bytes.Equal(after, before) {
				t.Fatalf("after Restore the file = %q, %v; want %q", after, readErr, before)
			}
		})
	}
}

// Building a carrier over a client calls WebSocketURL inside the
// computer's transaction, where its mutex may be held by a renewal waiting
// on that transaction's lock (attachedbackends computer_state.go). It must
// not take the mutex.
func TestTheCarrierBuildTakesNoClientLock(t *testing.T) {
	t.Parallel()
	client := redeemed(t, t.TempDir()).client
	client.mu.Lock()
	defer client.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := client.WebSocketURL()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WebSocketURL waited for the client's mutex")
	}
}

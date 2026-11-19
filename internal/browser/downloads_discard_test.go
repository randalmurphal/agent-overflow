package browser

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A download that ends keeping no file removes the file it was admitted
// with before its reservation settles: what the removal frees is charged
// to nothing, and what it could not free stays charged.

// unremovable puts a non-empty directory where the engine's file is, so
// its removal fails, and answers the bytes it occupies.
func unremovable(t *testing.T, file string) int64 {
	t.Helper()
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(file, "inside"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// awaitCharged waits for the quotas to hold workspace and artifacts bytes.
func awaitCharged(t *testing.T, m *Manager, authority *workspaceAuthority, workspace, artifacts int64) {
	t.Helper()
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		gotWorkspace, gotArtifacts := authority.downloadBytes.Used(), m.artifactBytes.Used()
		if gotWorkspace == workspace && gotArtifacts == artifacts {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the quotas hold %d and %d bytes, want %d and %d", gotWorkspace, gotArtifacts, workspace, artifacts)
		}
	}
}

// discardEnds are the ends of an artifact download that keep no file: the
// engine's own, the cap's (whose cancel runs on its own goroutine) and its
// page's close (stopEndedDownloads).
var discardEnds = []struct {
	name string
	end  func(t *testing.T, m *Manager, access Access)
}{
	{"the engine's failure", func(_ *testing.T, m *Manager, _ Access) {
		m.downloadProgress(downloadProgress{ID: "dl-1", Received: 3, State: downloadFailed, Error: "the network went away"})
	}},
	{"over the cap", func(_ *testing.T, m *Manager, _ Access) {
		m.downloadProgress(downloadProgress{ID: "dl-1", Received: maxDownloadBytes + 1, State: downloadInProgress})
	}},
	{"its page's close", func(t *testing.T, m *Manager, access Access) {
		if err := m.ClosePage(t.Context(), access, m.CompanionState(access.Caller()).Pages[0].ID); err != nil {
			t.Fatal(err)
		}
	}},
}

// A discard whose removal fails keeps the bytes left on disk charged to
// both quotas; one whose removal succeeds charges nothing.
func TestADiscardSettlesWhatItsRemovalLeft(t *testing.T) {
	for _, end := range discardEnds {
		t.Run(end.name+", removed", func(t *testing.T) {
			manager, access, file, workspace, artifacts := settleRig(t)
			authority := authorityOf(t, manager, access)
			end.end(t, manager, access)
			awaitRemoved(t, file)
			awaitCharged(t, manager, authority, workspace, artifacts)
		})
		t.Run(end.name+", left on disk", func(t *testing.T) {
			manager, access, file, workspace, artifacts := settleRig(t)
			authority := authorityOf(t, manager, access)
			left := unremovable(t, file)
			end.end(t, manager, access)
			awaitCharged(t, manager, authority, workspace+left, artifacts+left)
			if _, err := os.Lstat(file); err != nil {
				t.Fatalf("the file that could not be removed: %v", err)
			}
		})
	}
}

// moveJarDirectory points the open jar's directory elsewhere after its
// downloads were admitted, with a file there named as the engine id, and
// answers that file: a cleanup that derived a download's file again would
// remove it.
func moveJarDirectory(t *testing.T, m *Manager, id string) string {
	t.Helper()
	elsewhere := t.TempDir()
	decoy := filepath.Join(elsewhere, id)
	if err := os.WriteFile(decoy, []byte("not this download's"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	for _, scope := range m.scopes {
		scope.downloadDir = elsewhere
	}
	m.mu.Unlock()
	return decoy
}

// A row's discard removes the file named at its admission, never one
// derived again from its jar's directory.
func TestADiscardRemovesTheFileItsRowWasAdmittedWith(t *testing.T) {
	for _, end := range discardEnds {
		t.Run(end.name, func(t *testing.T) {
			manager, access, file, workspace, artifacts := settleRig(t)
			authority := authorityOf(t, manager, access)
			decoy := moveJarDirectory(t, manager, "dl-1")
			end.end(t, manager, access)
			awaitRemoved(t, file)
			awaitCharged(t, manager, authority, workspace, artifacts)
			if _, err := os.Stat(decoy); err != nil {
				t.Fatalf("the discard removed a file it was not admitted with: %v", err)
			}
		})
	}
}

// A parked download refused at its fate, and one dropped with the
// browser, remove the file named as they were parked.
func TestAParkedDiscardRemovesTheFileItWasParkedWith(t *testing.T) {
	for _, how := range []string{"refused at its fate", "dropped with the browser"} {
		t.Run(how, func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
			authority := authorityOf(t, manager, access)
			workspace, artifacts := authority.downloadBytes.Used(), manager.artifactBytes.Used()
			file := engineArtifact(t, manager, "dl-1")
			if !popupDownload(manager, engine, "popup", "dl-1", opener) {
				t.Fatal("the popup's download was refused")
			}
			decoy := moveJarDirectory(t, manager, "dl-1")
			if how == "refused at its fate" {
				manager.removeClosedPage(opener)
				manager.removeClosedPage("popup")
			} else if err := manager.Close(); err != nil {
				t.Fatal(err)
			}
			awaitRemoved(t, file)
			awaitCharged(t, manager, authority, workspace, artifacts)
			if _, err := os.Stat(decoy); err != nil {
				t.Fatalf("the discard removed a file it was not parked with: %v", err)
			}
		})
	}
}

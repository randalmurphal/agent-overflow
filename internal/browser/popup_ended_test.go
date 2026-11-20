package browser

import (
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// A popup never listed whose end the Manager knows (its close reported,
// its refusal) keeps its downloads with its nearest listed opener when
// that opener leaves before the popup's parked downloads settle: the
// opener's leave carries them, on every engine.

// listedPage answers the tab whose driver has handle.
func listedPage(t *testing.T, m *Manager, handle ids.PageHandle) *managedPage {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pageByHandleLocked(handle)
	if p == nil {
		t.Fatalf("no tab has handle %s", handle)
	}
	return p
}

// wantCarried waits until the left page holder lists download dl-1 of a
// popup it carried, canceled with its leave at 10 bytes.
func wantCarried(t *testing.T, holder *managedPage) {
	t.Helper()
	row := holderRow(t, holder, "dl-1")
	if row.State != downloadStateCanceled || row.Error != "page closed" || row.Bytes != 10 {
		t.Fatalf("the opener that left lists the popup's download as %s %q at %d bytes, want it canceled by its leave at 10", row.State, row.Error, row.Bytes)
	}
}

// awaitDone waits for a call run in the background to return.
func awaitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(hostedTestDeadline):
		t.Fatalf("%s never returned", what)
	}
}

// A popup being adopted whose close its engine reported, with the close's
// processing still queued, when its opener's tab closes: what it started
// before, and what it starts after its adoption ended and before that
// processing, go with the opener's leave.
func TestAClosedPopupsDownloadsGoWithItsOpenerThatLeavesBeforeTheCloseIsProcessed(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	release := engine.gate("popup")
	t.Cleanup(release)
	adopted := make(chan struct{})
	go func() {
		manager.adoptPopup(engine.popup(opener, "popup"))
		close(adopted)
	}()
	engine.awaitAttach(t, "popup")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, 10))
	processing := make(chan struct{})
	manager.pageEnds.report("popup", func() bool { return true }, func() { <-processing })
	manager.pageClosed("popup")
	holder := listedPage(t, manager, opener)
	if err := manager.CloseCompanionPage(t.Context(), access, holder.id); err != nil {
		t.Fatalf("close the opener: %v", err)
	}
	release()
	awaitDone(t, adopted, "the adoption")
	if !popupDownload(manager, engine, "popup", "dl-2", opener) {
		t.Fatal("a download of the closed popup after its adoption ended was refused")
	}
	close(processing)
	wantCarried(t, holder)
	if row := holderRow(t, holder, "dl-2"); row.State != downloadStateCanceled || row.Error != "page closed" {
		t.Fatalf("the opener that left lists the popup's later download as %s %q, want it canceled by its leave", row.State, row.Error)
	}
}

// A popup the Manager refuses, whose discard is under way when its
// opener's tab closes: refused at its attach, or at its reservation.
func TestARefusedPopupsDownloadsGoWithItsOpenerThatLeavesDuringItsDiscard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		popup func(e *adoptionEngine, opener ids.PageHandle) enginePopup
	}{
		{"its attach failed", func(e *adoptionEngine, opener ids.PageHandle) enginePopup {
			e.failing["popup"] = true
			return e.popup(opener, "popup")
		}},
		{"its reservation found no jar of its profile", func(_ *adoptionEngine, opener ids.PageHandle) enginePopup {
			return enginePopup{Profile: "another-profile", Opener: opener, Handle: "popup"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManager(t)
			if !popupDownload(manager, engine, "popup", "dl-1", opener) {
				t.Fatal("the popup's download was refused")
			}
			manager.downloadProgress(progressOf("dl-1", downloadInProgress, 10))
			discarding, release := engine.holdDiscards(t)
			popup := tc.popup(engine, opener)
			adopted := make(chan struct{})
			go func() {
				manager.adoptPopup(popup)
				close(adopted)
			}()
			select {
			case <-discarding:
			case <-time.After(hostedTestDeadline):
				t.Fatal("the popup was never discarded")
			}
			holder := listedPage(t, manager, opener)
			if err := manager.CloseCompanionPage(t.Context(), access, holder.id); err != nil {
				t.Fatalf("close the opener: %v", err)
			}
			release()
			awaitDone(t, adopted, "the adoption")
			wantCarried(t, holder)
			manager.mu.Lock()
			ended, carried := manager.endedPopupsLocked().len(), manager.carriersLocked().len()
			manager.mu.Unlock()
			if ended != 0 || carried != 0 {
				t.Fatalf("the Manager keeps %d popup ends and %d carriers past the discard", ended, carried)
			}
		})
	}
}

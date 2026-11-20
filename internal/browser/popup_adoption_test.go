package browser

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// A popup opened by a popup still being adopted waits for that adoption
// and joins its tabs; a download it started meanwhile is in its own tab.
func TestNestedPopupBeforeParentListed(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	release := engine.gate("parent")
	defer release()
	parentDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup(opener, "parent")); close(parentDone) }()
	engine.awaitAttach(t, "parent")
	popupDownload(manager, engine, "child", "dl-1", "parent", opener)
	childDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup("parent", "child")); close(childDone) }()
	select {
	case <-childDone:
		t.Fatal("the child's adoption ended before its opener's")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-parentDone
	<-childDone
	if !listed(manager, "parent") || !listed(manager, "child") {
		t.Fatalf("listed parent %v child %v", listed(manager, "parent"), listed(manager, "child"))
	}
	if n := len(manager.ownedPages(access.tb)); n != 3 {
		t.Fatalf("the thread has %d tabs, want 3", n)
	}
	wantDownloads(t, manager, "child", "dl-1 in_progress 0")
	if got := engine.discards(); len(got) != 0 {
		t.Fatalf("discarded %v", got)
	}
}

// A popup opened by a popup the Manager discards is discarded too, and its
// downloads go to the nearest opener still listed.
func TestAPopupOfADiscardedPopupIsDiscarded(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	engine.failing["parent"] = true
	release := engine.gate("parent")
	defer release()
	parentDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup(opener, "parent")); close(parentDone) }()
	engine.awaitAttach(t, "parent")
	popupDownload(manager, engine, "child", "dl-1", "parent", opener)
	childDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup("parent", "child")); close(childDone) }()
	release()
	<-parentDone
	// At the parent's discard, not after popupOpenerWait.
	awaitPromptly(t, childDone)
	if got := engine.discards(); !slices.Equal(got, []ids.PageHandle{"parent", "child"}) {
		t.Fatalf("discarded %v, want parent then child", got)
	}
	wantDownloads(t, manager, opener, "dl-1 in_progress 0")
}

// A popup whose opener is still being created or restored waits for its
// opener's listing and goes on as it is listed, or is discarded as its
// opener's creation or restore closes, not at the opener wait's bound: any
// page joining the tabs, or a creation or restore that will not, wakes the
// waiters, not only a finished adoption.
func TestAPopupOfAnOpenerNotListedYetGoesOnAtItsListing(t *testing.T) {
	for _, name := range []string{"created", "restored", "closed while created", "closed while restored"} {
		restored := name == "restored" || name == "closed while restored"
		closing := name == "closed while created" || name == "closed while restored"
		t.Run(name, func(t *testing.T) {
			manager, engine := newLifecycleManager(t)
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			var pageID string
			if restored {
				page := mustOpen(t, manager, access, "https://example.test/")
				p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
				if err != nil {
					t.Fatal(err)
				}
				if moved, err := manager.suspendPage(t.Context(), p); err != nil || !moved {
					t.Fatalf("suspend: %v, %v", moved, err)
				}
				pageID = page.ID
			}
			handles := make(chan string, 1)
			hold := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(hold) }) }
			defer release()
			// The engine page exists and the tab is not listed yet.
			engine.onNewFakePage = func(page *fakePage) {
				handles <- string(page.handle)
				<-hold
			}
			opened := make(chan error, 1)
			go func() {
				var err error
				if restored {
					_, err = manager.Pages(context.Background(), access)
					if err == nil {
						_, _, err = manager.Evaluate(context.Background(), access, PageID(pageID), "1", nil)
					}
				} else {
					_, err = manager.Open(context.Background(), access, "https://example.test/", OpenOptions{})
				}
				opened <- err
			}()
			var opener string
			select {
			case opener = <-handles:
			case <-time.After(hostedTestDeadline):
				t.Fatal("the opener's engine page was never made")
			}
			listed := make(chan bool, 1)
			go func() { listed <- manager.awaitOpener(ids.PageHandle(opener)) }()
			time.Sleep(50 * time.Millisecond)
			if closing && restored {
				// The restore finds its record gone and abandons the page.
				if err := manager.ClosePage(context.Background(), access, PageID(pageID)); err != nil {
					t.Fatalf("close the restoring tab: %v", err)
				}
			} else if closing {
				tabs := manager.CompanionState(access.Caller()).Pages
				if len(tabs) != 1 {
					t.Fatalf("tabs %+v", tabs)
				}
				// The close waits for the creation it marks.
				go func() { _ = manager.ClosePage(context.Background(), access, PageID(tabs[0].ID)) }()
				for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
					manager.mu.Lock()
					pending := manager.pending[PageID(tabs[0].ID)]
					marked := pending != nil && pending.closed
					manager.mu.Unlock()
					if marked {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the creating tab was never closed")
					}
				}
			}
			start := time.Now()
			release()
			if err := <-opened; (err != nil) != closing {
				t.Fatalf("open: %v", err)
			}
			select {
			case ok := <-listed:
				if ok == closing {
					t.Fatalf("the popup's opener was taken as listed %v", ok)
				}
			case <-time.After(popupOpenerWait / 4):
				t.Fatalf("the popup still waited for its opener %v after the opener was listed or closed", time.Since(start))
			}
		})
	}
}

// A popup whose opener left before the popup's report is handled is
// discarded at once.
func TestAPopupOfAnOpenerThatLeftIsDiscardedAtOnce(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	manager.removeClosedPage(opener)
	done := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup(opener, "popup")); close(done) }()
	awaitPromptly(t, done)
	if got := engine.discards(); !slices.Equal(got, []ids.PageHandle{"popup"}) {
		t.Fatalf("discarded %v", got)
	}
}

// A popup reported before its opener, as goroutines may deliver them,
// waits for its opener to be reported and adopted.
func TestAPopupReportedBeforeItsOpenerWaitsForIt(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	childDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup("parent", "child")); close(childDone) }()
	for deadline := time.Now().Add(hostedTestDeadline); ; {
		manager.mu.Lock()
		_, adopting := manager.adopting["child"]
		manager.mu.Unlock()
		if adopting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child's adoption never started")
		}
		time.Sleep(time.Millisecond)
	}
	manager.adoptPopup(engine.popup(opener, "parent"))
	<-childDone
	if n := len(manager.ownedPages(access.tb)); n != 3 {
		t.Fatalf("the thread has %d tabs, want 3", n)
	}
}

// The fates the Manager remembers are bounded.
func TestPopupFatesAreBounded(t *testing.T) {
	manager, _, _, opener := newAdoptionManager(t)
	for i := range 4 * orderSlack {
		manager.resolveParked(ids.PageHandle(fmt.Sprintf("popup-%d", i)), nil, opener)
	}
	manager.parkMu.Lock()
	defer manager.parkMu.Unlock()
	if n := manager.popupFates.len(); n > orderSlack {
		t.Fatalf("%d fates kept", n)
	}
}

// A refused popup lets go of the address and title held for it, and one
// reported after its refusal is not held: however many popups a thread
// has refused, another popup's held report survives to its listing.
func TestARefusedPopupHoldsNoReport(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	manager.updatePageInfo("keep", "https://example.test/kept", "Kept", 1)
	for i := range 2 * orderSlack {
		handle := ids.PageHandle(fmt.Sprintf("refused-%d", i))
		engine.failing[handle] = true
		manager.updatePageInfo(handle, "https://example.test/refused", "", uint64(2+i))
		manager.adoptPopup(engine.popup(opener, handle))
		manager.updatePageInfo(handle, "https://example.test/late", "", uint64(2+i))
	}
	manager.mu.Lock()
	held := manager.heldInfo.len()
	manager.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d reports held, want the pending popup's alone", held)
	}
	manager.adoptPopup(engine.popup(opener, "keep"))
	if info := pageInfoOf(manager, access, "keep"); info.URL != "https://example.test/kept" || info.Title != "Kept" {
		t.Fatalf("the popup lists %+v, want its held report", info)
	}
}

// A popup listed with a report held for it is pushed once, with that
// report.
func TestAPopupListedWithAHeldReportIsPushedOnce(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	var mu sync.Mutex
	var pushed []CompanionEvent
	manager.SetEventSink(func(event CompanionEvent) {
		if ThreadID(event.ThreadID) == access.ThreadID() && event.Kind == "state" {
			mu.Lock()
			pushed = append(pushed, event)
			mu.Unlock()
		}
	})
	manager.updatePageInfo("popup", "https://example.test/held", "Held", 1)
	manager.adoptPopup(engine.popup(opener, "popup"))
	mu.Lock()
	defer mu.Unlock()
	if len(pushed) != 1 {
		t.Fatalf("listing the popup pushed %d states, want 1", len(pushed))
	}
	for _, page := range pushed[0].Pages {
		if page.URL == "https://example.test/held" && page.Title == "Held" {
			return
		}
	}
	t.Fatalf("the push lists %+v, without the popup's held report", pushed[0].Pages)
}

// A popup that navigated before its listing is listed, in its one push,
// with Back as its history has it.
func TestAPopupIsListedWithItsHistory(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	engine.histories["popup"] = []string{"https://example.test/first", "https://example.test/second"}
	var mu sync.Mutex
	var pushed []CompanionEvent
	manager.SetEventSink(func(event CompanionEvent) {
		if ThreadID(event.ThreadID) == access.ThreadID() && event.Kind == "state" {
			mu.Lock()
			pushed = append(pushed, event)
			mu.Unlock()
		}
	})
	manager.updatePageInfo("popup", "https://example.test/second", "Second", 1)
	manager.adoptPopup(engine.popup(opener, "popup"))
	mu.Lock()
	defer mu.Unlock()
	if len(pushed) != 1 {
		t.Fatalf("listing the popup pushed %d states, want 1", len(pushed))
	}
	for _, page := range pushed[0].Pages {
		if page.URL == "https://example.test/second" {
			if !page.CanGoBack || page.CanGoForward {
				t.Fatalf("the popup is listed with back %v, forward %v", page.CanGoBack, page.CanGoForward)
			}
			return
		}
	}
	t.Fatalf("the push lists %+v, without the popup", pushed[0].Pages)
}

// A popup being adopted holds its jar's profile: its opener's close, the
// jar's last listed page, leaves the profile running until the adoption
// ends, which then disposes it.
func TestAPopupBeingAdoptedHoldsItsProfile(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	manager.mu.Lock()
	var profile *adoptionProfile
	for _, scope := range manager.scopes {
		profile = scope.profile.(*adoptionProfile)
	}
	manager.mu.Unlock()
	disposed := func() bool {
		profile.mu.Lock()
		defer profile.mu.Unlock()
		return profile.disposed
	}
	release := engine.gate("popup")
	defer release()
	done := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup(opener, "popup")); close(done) }()
	engine.awaitAttach(t, "popup")
	tabs := manager.CompanionState(access.Caller()).Pages
	if len(tabs) != 1 {
		t.Fatalf("the thread has %d tabs, want the opener", len(tabs))
	}
	if err := manager.ClosePage(t.Context(), access, PageID(tabs[0].ID)); err != nil {
		t.Fatal(err)
	}
	if disposed() {
		t.Fatal("the opener's close disposed the profile the popup is being attached in")
	}
	release()
	<-done
	if listed(manager, "popup") {
		t.Fatal("a popup whose opener closed was listed")
	}
	deadline := time.Now().Add(hostedTestDeadline)
	for !disposed() {
		if time.Now().After(deadline) {
			t.Fatal("the profile was never disposed once the adoption ended")
		}
		time.Sleep(time.Millisecond)
	}
	manager.mu.Lock()
	scopes := len(manager.scopes)
	manager.mu.Unlock()
	if scopes != 0 {
		t.Fatalf("%d jars are still registered", scopes)
	}
}

// A popup being adopted when its opener, the thread's last private tab,
// closes can no longer be listed, so it keeps the closed tab's private jar
// from no new tab: the thread's next private tab opens a new jar, and the
// old profile stays until the adoption ends.
func TestAPopupBeingAdoptedKeepsNoPrivateJarForANewTab(t *testing.T) {
	manager, engine, access, _ := newAdoptionManager(t)
	setJar(t, manager, access, JarPrivate)
	first := mustOpen(t, manager, access, "https://example.test/private")
	old := pageJar(t, manager, first.ID)
	manager.mu.Lock()
	oldScope := manager.scopes[jarKey(old)]
	manager.mu.Unlock()
	release := engine.gate("popup")
	defer release()
	done := make(chan struct{})
	go func() {
		manager.adoptPopup(engine.popup(driverHandle(manager, access, first.ID), "popup"))
		close(done)
	}()
	engine.awaitAttach(t, "popup")
	if err := manager.ClosePage(t.Context(), access, PageID(first.ID)); err != nil {
		t.Fatal(err)
	}
	next := pageJar(t, manager, mustOpen(t, manager, access, "https://example.test/next").ID)
	if next == old {
		t.Fatalf("a private tab opened while a popup of the closed one was adopted reuses its jar %q", old)
	}
	if oldScope.profile.(*adoptionProfile).isDisposed() {
		t.Fatal("the old private jar was disposed while a popup was being attached in it")
	}
	release()
	<-done
	if listed(manager, "popup") {
		t.Fatal("a popup whose opener closed was listed")
	}
	eventually(t, "the old private jar is disposed", oldScope.profile.(*adoptionProfile).isDisposed)
}

// Popups being adopted count against the caps from their admission: a
// burst of them past the thread's cap is refused at once rather than
// attached and dropped at its listing.
func TestPopupsBeingAdoptedCountAgainstTheCaps(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	var releases []func()
	var dones []chan struct{}
	for i := range maxPagesPerThread {
		handle := ids.PageHandle(fmt.Sprintf("popup-%d", i))
		releases = append(releases, engine.gate(handle))
		done := make(chan struct{})
		dones = append(dones, done)
		go func() { manager.adoptPopup(engine.popup(opener, handle)); close(done) }()
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	// The opener is one page, so all but one popup is admitted.
	for range maxPagesPerThread - 1 {
		select {
		case <-engine.reached:
		case <-time.After(hostedTestDeadline):
			t.Fatal("an admitted popup was never attached")
		}
	}
	deadline := time.Now().Add(hostedTestDeadline)
	for len(engine.discards()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("discarded %v while the others are attached, want the one past the cap", engine.discards())
		}
		time.Sleep(time.Millisecond)
	}
	for _, release := range releases {
		release()
	}
	for _, done := range dones {
		<-done
	}
	if got := len(manager.CompanionState(access.Caller()).Pages); got != maxPagesPerThread {
		t.Fatalf("the thread has %d tabs, want its cap of %d", got, maxPagesPerThread)
	}
}

// A popup whose discard the engine never answers ends its adoption at the
// discard's deadline: its reservation goes, so its profile and the caps
// are free again, and its downloads go to its opener.
func TestAPopupWhoseDiscardNeverAnswersEndsItsReservation(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	engine.failing["popup"] = true
	engine.stallDiscard = true
	popupDownload(manager, engine, "popup", "dl-1")
	done := make(chan struct{})
	go func() {
		manager.adoptPopup(engine.popup(opener, "popup"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(popupDiscardTimeout + hostedTestDeadline):
		t.Fatal("the adoption never ended")
	}
	manager.mu.Lock()
	reserved := 0
	for _, scope := range manager.scopes {
		reserved += len(scope.creating)
	}
	adopting := len(manager.adopting)
	manager.mu.Unlock()
	if reserved != 0 || adopting != 0 {
		t.Fatalf("%d reservations and %d adoptions remain, want none", reserved, adopting)
	}
	wantDownloads(t, manager, opener, "dl-1 in_progress 0")
}

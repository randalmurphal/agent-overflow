package browser

import (
	"errors"
	"os"
	"sync"
	"testing"
)

// Every move of a download row's phase, from every phase: advance allows
// only the moves of its lifecycle and settle ends a row once.
func TestADownloadRowMovesOnlyByItsLifecycle(t *testing.T) {
	phases := []downloadRowPhase{rowRunning, rowCancelRequested, rowPublishing, rowEndPending, rowEndPendingCancelRequested, rowSettled, rowUserCanceled}
	allowed := map[[2]downloadRowPhase]bool{
		{rowRunning, rowCancelRequested}: true,
		{rowRunning, rowPublishing}:      true,
		{rowCancelRequested, rowRunning}: true,
		// A finish that beat the cancel is published.
		{rowCancelRequested, rowPublishing}: true,
		// Its page left after its end was received, which settles it.
		{rowRunning, rowEndPending}:                        true,
		{rowCancelRequested, rowEndPendingCancelRequested}: true,
	}
	for _, from := range phases {
		for _, to := range phases {
			entry := DownloadInfo{ID: "dl", phase: from}
			err := entry.advance(to)
			if want := allowed[[2]downloadRowPhase{from, to}]; want != (err == nil) {
				t.Errorf("%v to %v answered %v", from, to, err)
			} else if err != nil && (!errors.Is(err, errDownloadPhase) || entry.phase != from) {
				t.Errorf("a refused %v to %v answered %v and left %v", from, to, err, entry.phase)
			} else if err == nil && entry.phase != to {
				t.Errorf("an allowed %v to %v left %v", from, to, entry.phase)
			}
		}
	}
	for _, tc := range []struct {
		from  downloadRowPhase
		state string
		want  downloadRowPhase
	}{
		{rowRunning, downloadStateCanceled, rowSettled},
		{rowRunning, downloadStateFailed, rowSettled},
		{rowCancelRequested, downloadStateCanceled, rowUserCanceled},
		{rowCancelRequested, downloadStateFailed, rowSettled},
		{rowCancelRequested, downloadStateCompleted, rowSettled},
		{rowPublishing, downloadStateCompleted, rowSettled},
		{rowPublishing, downloadStateFailed, rowSettled},
		{rowEndPending, downloadStateCompleted, rowSettled},
		{rowEndPending, downloadStateCanceled, rowSettled},
		{rowEndPendingCancelRequested, downloadStateCanceled, rowUserCanceled},
		{rowEndPendingCancelRequested, downloadStateCompleted, rowSettled},
	} {
		entry := DownloadInfo{ID: "dl", phase: tc.from, State: downloadStateRunning}
		if err := entry.settle(tc.state); err != nil || entry.phase != tc.want || entry.State != tc.state {
			t.Errorf("settling %v as %s answered %v, left %v %s; want %v", tc.from, tc.state, err, entry.phase, entry.State, tc.want)
		}
		if err := entry.settle(tc.state); !errors.Is(err, errDownloadPhase) || entry.phase != tc.want {
			t.Errorf("settling %v twice answered %v", tc.from, err)
		}
	}
	entry := DownloadInfo{ID: "dl", State: downloadStateRunning}
	if err := entry.settle(downloadStateRunning); !errors.Is(err, errDownloadPhase) || entry.phase != rowRunning {
		t.Errorf("a settle as running answered %v", err)
	}
}

// A publish settles the row it began from only while that row is being
// published, and answers why not otherwise: no row, or a row in another
// phase.
func TestAPublishFindsOnlyItsPublishingRow(t *testing.T) {
	log := &pageDownloadLog{}
	held := log.hold()
	defer held.release()
	if _, err := held.publishing("dl"); !errors.Is(err, errDownloadPhase) {
		t.Fatalf("a publish of a row not in the log answered %v", err)
	}
	entry := held.add(DownloadInfo{ID: "dl", State: downloadStateRunning})
	if _, err := held.publishing("dl"); !errors.Is(err, errDownloadPhase) {
		t.Fatalf("a publish of a running row answered %v", err)
	}
	if err := entry.advance(rowPublishing); err != nil {
		t.Fatal(err)
	}
	if got, err := held.publishing("dl"); err != nil || got != entry {
		t.Fatalf("a publish of its publishing row answered %p, %v, want %p", got, err, entry)
	}
}

// cancelCountingProfile records the cancels the Manager sends and answers each
// as taken, without ending the download, as an engine whose end report
// is still on the way.
type cancelCountingProfile struct {
	engineProfile
	mu      sync.Mutex
	cancels []downloadID
}

func (p *cancelCountingProfile) CancelDownload(id downloadID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels = append(p.cancels, id)
	return nil
}

func (p *cancelCountingProfile) sent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.cancels)
}

// countCancels puts a cancelCountingProfile in front of the open jar's profile.
func countCancels(t *testing.T, m *Manager) *cancelCountingProfile {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		counting := &cancelCountingProfile{engineProfile: scope.profile}
		scope.profile = counting
		return counting
	}
	t.Fatal("no open jar")
	return nil
}

// A cancel made while its finished file is published is no cancel: the
// engine is not asked, and the publish completes the row, announced.
func TestACancelDuringThePublishIsNotSent(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	linking, release := make(chan struct{}), make(chan struct{})
	linkDownload = func(staged, candidate string) error {
		if !placing(candidate) {
			close(linking)
			<-release
		}
		return os.Link(staged, candidate)
	}
	t.Cleanup(func() { linkDownload = os.Link })
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	// Before the Manager's Close, which waits for the publish.
	t.Cleanup(unblock)
	startDownload(t, manager, access, page, "late.txt", 10, "hold")
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	profile := fakeProfileOf(t, manager)
	counting := countCancels(t, manager)
	finished := make(chan error, 1)
	go func() { finished <- profile.finishHeld(item.engineID) }()
	<-linking
	log.take()
	if err := manager.CancelThreadDownload(access, DownloadKey(item.ID)); err != nil {
		t.Fatalf("a cancel during the publish answered %v", err)
	}
	if n := counting.sent(); n != 0 {
		t.Fatalf("a cancel during the publish reached the engine %d times", n)
	}
	unblock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCompleted {
		t.Fatalf("the published download lists as %+v, want completed", got)
	}
	if n := countKind(log.take(), "download"); n != 1 {
		t.Fatalf("the download was announced %d times, want once", n)
	}
}

// A second cancel while the first is with the engine is not sent; once the
// engine refuses the first, the next is, and the engine's cancel ends the
// row unannounced.
func TestACancelIsSentOnceUntilItsRefusal(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "slow.bin", 4096, "hold")
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	counting := countCancels(t, manager)
	log.take()
	for range 2 {
		if err := manager.CancelThreadDownload(access, DownloadKey(item.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if n := counting.sent(); n != 1 {
		t.Fatalf("two cancels reached the engine %d times, want once", n)
	}
	manager.downloadProgress(downloadProgress{ID: item.engineID, State: downloadCancelRefused, Error: "refused"})
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateRunning || got.Error != "refused" {
		t.Fatalf("after the refusal the download lists as %+v", got)
	}
	if err := manager.CancelThreadDownload(access, DownloadKey(item.ID)); err != nil {
		t.Fatal(err)
	}
	if n := counting.sent(); n != 2 {
		t.Fatalf("the cancel after a refusal reached the engine %d times in all, want twice", n)
	}
	manager.downloadProgress(downloadProgress{ID: item.engineID, State: downloadCanceled})
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCanceled {
		t.Fatalf("the canceled download lists as %+v", got)
	}
	if n := countKind(log.take(), "download"); n != 0 {
		t.Fatalf("the user's own cancel was announced %d times", n)
	}
}

// A download whose cancel the engine refused finishes and is published as
// any: completed, announced.
func TestAPublishAfterARefusedCancelCompletes(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "kept.txt", 10, "hold")
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	profile := fakeProfileOf(t, manager)
	countCancels(t, manager)
	if err := manager.CancelThreadDownload(access, DownloadKey(item.ID)); err != nil {
		t.Fatal(err)
	}
	manager.downloadProgress(downloadProgress{ID: item.engineID, State: downloadCancelRefused, Error: "refused"})
	log.take()
	if err := profile.finishHeld(item.engineID); err != nil {
		t.Fatal(err)
	}
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateCompleted || !got.Openable || got.Error != "" {
		t.Fatalf("the download lists as %+v, want completed", got)
	}
	if n := countKind(log.take(), "download"); n != 1 {
		t.Fatalf("the download was announced %d times, want once", n)
	}
}

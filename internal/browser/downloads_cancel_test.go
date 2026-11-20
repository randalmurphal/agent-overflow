package browser

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// The user's cancel stops a running download, keeps no partial file, gives
// any reservation back, and is not announced.
func TestCancelingADownloadSettlesItAsCanceled(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			testCancelingADownloadSettlesItAsCanceled(t, mode.saving)
		})
	}
}

func testCancelingADownloadSettlesItAsCanceled(t *testing.T, saving downloadSaving) {
	manager, log, access, page := newDownloadManagerFor(t, saving)
	startDownload(t, manager, access, page, "big.zip", 4096, "hold")
	listed := threadDownloads(manager, access)
	if len(listed) != 1 || listed[0].State != downloadStateRunning || listed[0].ReceivedBytes != 2048 || listed[0].TotalBytes != 4096 {
		t.Fatalf("a held download lists as %+v", listed)
	}
	item, err := manager.threadDownload(access, DownloadKey(listed[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := manager.pageForDownload(item.engineID)
	partial := heldFile(t, manager, item.engineID)
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("the held download has no partial file: %v", err)
	}
	log.take()

	if err := manager.CancelThreadDownload(access, DownloadKey(listed[0].ID)); err != nil {
		t.Fatal(err)
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCanceled || got.Error != "" {
		t.Fatalf("a canceled download lists as %+v", got)
	}
	if n := countKind(log.take(), "download"); n != 0 {
		t.Fatalf("the user's cancel was announced %d times", n)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial file is still there: %v", err)
	}
	if reserved := p.authority.downloadBytes.Used(); reserved != 0 {
		t.Fatalf("the workspace still reserves %d bytes", reserved)
	}
	// A second cancel of a settled download is no error and changes nothing.
	if err := manager.CancelThreadDownload(access, DownloadKey(listed[0].ID)); err != nil {
		t.Fatalf("a repeated cancel: %v", err)
	}
	if err := manager.CancelThreadDownload(access, "d-nonexistent"); !errors.Is(err, ErrDownloadUnavailable) {
		t.Fatalf("a cancel of an unknown download answers %v", err)
	}
}

// A cancel that reaches the engine after it let the finished download go,
// and before it reported the finish, loses: the finish settles the download
// and its file is kept. The engine's errDownloadEnded is that answer.
func TestACancelRacingTheFinishKeepsTheFinishedFile(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "late.bin", 4096, "hold")
	listed := threadDownloads(manager, access)
	item, err := manager.threadDownload(access, DownloadKey(listed[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	log.take()
	engine := manager.engine.(*fakeEngine)
	engine.downloadReleased = func(downloadID) {
		if err := manager.CancelThreadDownload(access, DownloadKey(listed[0].ID)); err != nil {
			t.Errorf("a cancel that lost to the finish answers %v", err)
		}
		if got := threadDownloads(manager, access)[0]; got.State != downloadStateRunning {
			t.Errorf("a cancel that lost settled the download as %+v before the engine's report", got)
		}
	}
	if err := fakeProfileOf(t, manager).finishHeld(item.engineID); err != nil {
		t.Fatal(err)
	}
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateCompleted || got.ReceivedBytes != 4096 {
		t.Fatalf("the download lists as %+v, want completed", got)
	}
	finished, err := manager.finishedDownloadPath(access, DownloadKey(got.ID))
	if err != nil {
		t.Fatalf("the finished file: %v", err)
	}
	if stat, err := os.Stat(finished.path); err != nil || stat.Size() != 4096 {
		t.Fatalf("the finished file at %s: %v", finished.path, err)
	}
	if n := countKind(log.take(), "download"); n != 1 {
		t.Fatalf("the finish that beat the cancel was announced %d times, want once", n)
	}
}

type cancelRefusingEngine struct {
	*fakeEngine
	refuse error
}

func (e *cancelRefusingEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &cancelRefusingProfile{fakeProfile: profile.(*fakeProfile), refuse: e.refuse}, nil
}

type cancelRefusingProfile struct {
	*fakeProfile
	refuse error
}

func (p *cancelRefusingProfile) CancelDownload(downloadID) error { return p.refuse }

// An engine that refuses the cancel answers the user with its refusal, and
// the download stays running: the engine's later report of its end is a
// failure, not the user's cancel.
func TestARefusedCancelReachesTheUserAndTheDownloadRuns(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	engine := &cancelRefusingEngine{fakeEngine: manager.engine.(*fakeEngine), refuse: errors.New("Browser.cancelDownload wasn't found")}
	manager.engine = engine
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	frame := paneHandle(manager, access, page.ID)
	manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "G", SuggestedName: "a.bin"})
	id := threadDownloads(manager, access)[0].ID

	err = manager.CancelThreadDownload(access, DownloadKey(id))
	if err == nil || !strings.Contains(err.Error(), "cancelDownload wasn't found") {
		t.Fatalf("a refused cancel answers %v, want the engine's refusal", err)
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateRunning {
		t.Fatalf("after a refused cancel the download lists as %+v, want running", got)
	}
	manager.downloadProgress(downloadProgress{ID: "G", State: downloadFailed})
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateFailed {
		t.Fatalf("an end after a refused cancel lists as %+v, want failed", got)
	}
}

// An engine that takes the cancel and refuses it later (the hosted engine's
// launcher) withdraws it: the download runs on, listed with the refusal,
// its end reads as the engine's own, and the user can cancel again. The
// refusal does not outlive the download's end.
func TestALaterCancelRefusalWithdrawsTheCancel(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	manager.engine = &cancelRefusingEngine{fakeEngine: manager.engine.(*fakeEngine)}
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	frame := paneHandle(manager, access, page.ID)
	const refusal = "the browser refused to cancel the download: Unspecified error"
	refuse := func(engineID downloadID) {
		t.Helper()
		row := func() ThreadDownload {
			for _, d := range threadDownloads(manager, access) {
				if item, err := manager.threadDownload(access, DownloadKey(d.ID)); err == nil && item.engineID == engineID {
					return d
				}
			}
			t.Fatalf("no row for %s", engineID)
			return ThreadDownload{}
		}
		if err := manager.CancelThreadDownload(access, DownloadKey(row().ID)); err != nil {
			t.Fatal(err)
		}
		manager.downloadProgress(downloadProgress{ID: engineID, State: downloadCancelRefused, Error: refusal})
		manager.downloadProgress(downloadProgress{ID: engineID, State: downloadInProgress, Received: 10})
		if got := row(); got.State != downloadStateRunning || got.Error != refusal {
			t.Fatalf("after a refused cancel the download lists as %+v, want running with the refusal", got)
		}
	}

	manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "G1", SuggestedName: "a.bin"})
	refuse("G1")
	manager.downloadProgress(downloadProgress{ID: "G1", State: downloadFailed})
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateFailed || got.Error != "the download stopped before it finished" {
		t.Fatalf("an end after a refused cancel lists as %+v, want failed", got)
	}

	manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "G2", SuggestedName: "b.bin"})
	refuse("G2")
	id := threadDownloads(manager, access)[0].ID
	if err := manager.CancelThreadDownload(access, DownloadKey(id)); err != nil {
		t.Fatal(err)
	}
	manager.downloadProgress(downloadProgress{ID: "G2", State: downloadCanceled})
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCanceled || got.Error != "" {
		t.Fatalf("a second cancel that won lists as %+v, want canceled", got)
	}

	manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "G3", SuggestedName: "c.bin"})
	refuse("G3")
	manager.downloadProgress(downloadProgress{ID: "G3", State: downloadFailed, Error: "the network failed"})
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateFailed || got.Error != "the network failed" {
		t.Fatalf("a failure after a refused cancel lists as %+v", got)
	}

	manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "G4", SuggestedName: "d.bin"})
	refuse("G4")
	manager.downloadProgress(downloadProgress{ID: "G4", State: downloadPageGone})
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCanceled || got.Error != "" {
		t.Fatalf("an end with no reason of its own after a refused cancel lists as %+v", got)
	}
}

// A download that finishes after a refused cancel lists no error.
func TestAFinishAfterARefusedCancelListsNoRefusal(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, mode.saving)
			startDownload(t, manager, access, page, "late.bin", 4096, "hold")
			listed := threadDownloads(manager, access)
			item, err := manager.threadDownload(access, DownloadKey(listed[0].ID))
			if err != nil {
				t.Fatal(err)
			}
			manager.downloadProgress(downloadProgress{ID: item.engineID, State: downloadCancelRefused, Error: "refused"})
			if got := threadDownloads(manager, access)[0]; got.Error != "refused" {
				t.Fatalf("the refusal lists as %+v", got)
			}
			if err := fakeProfileOf(t, manager).finishHeld(item.engineID); err != nil {
				t.Fatal(err)
			}
			if got := threadDownloads(manager, access)[0]; got.State != downloadStateCompleted || got.Error != "" {
				t.Fatalf("the finish lists as %+v", got)
			}
		})
	}
}

// A second cancel that reaches the engine after it released its record and
// before it reported the first cancel does not withdraw the first: the
// download ends canceled, unannounced, with its partial file removed.
func TestASecondCancelKeepsTheFirstCancel(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "held.bin", 4096, "hold")
	listed := threadDownloads(manager, access)
	item, err := manager.threadDownload(access, DownloadKey(listed[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	partial := heldFile(t, manager, item.engineID)
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("no partial file: %v", err)
	}
	log.take()
	manager.engine.(*fakeEngine).downloadReleased = func(downloadID) {
		if err := manager.CancelThreadDownload(access, DownloadKey(listed[0].ID)); err != nil {
			t.Errorf("the second cancel answers %v", err)
		}
	}
	if err := manager.CancelThreadDownload(access, DownloadKey(listed[0].ID)); err != nil {
		t.Fatal(err)
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCanceled || got.Error != "" {
		t.Fatalf("the download lists as %+v, want canceled", got)
	}
	if n := countKind(log.take(), "download"); n != 0 {
		t.Fatalf("the user's cancel was announced %d times", n)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial file stays: %v", err)
	}
}

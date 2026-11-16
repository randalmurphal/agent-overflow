package threadapp

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/store"
)

// commitDelete claims and commits the delete of ids as one family delete
// would, so TearDown and Drop see what they run on.
func commitDelete(t *testing.T, database *store.Store, ids ...string) {
	t.Helper()
	if _, err := database.ClaimDeleteFamily("op", ids, nil); err != nil {
		t.Fatalf("ClaimDeleteFamily: %v", err)
	}
	if _, err := database.CommitDeleteClaim("op", store.DeleteWorktrees{}); err != nil {
		t.Fatalf("CommitDeleteClaim: %v", err)
	}
}

// destroy is a committed thread's destroy: TearDown, then Drop.
func destroy(service *Service, threadID string, ports DeletePorts) error {
	if err := service.TearDown(threadID, ports); err != nil {
		return err
	}
	return service.Drop(threadID, ports)
}

func TestDestroyPreservesResourceOrder(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	commitDelete(t, database, thread.ID)

	var calls []string
	ports := DeletePorts{
		CleanProviderBackground: func(store.Thread) error { calls = append(calls, "background"); return nil },
		StopSession:             func(string) error { calls = append(calls, "session"); return nil },
		CancelWorktreeSetup:     func(string) { calls = append(calls, "setup") },
		CancelShellCommand:      func(string) { calls = append(calls, "shell") },
		CloseTerminals:          func(string) error { calls = append(calls, "terminal"); return nil },
		CloseBrowserPages:       func(string) error { calls = append(calls, "browser"); return nil },
		ClearAutoReconnect:      func(string) { calls = append(calls, "reconnect") },
		CleanupAttachments:      func(string) error { calls = append(calls, "attachments"); return nil },
		CleanupReplayLog:        func(string) error { calls = append(calls, "replay"); return nil },
		CleanupMemoryRecords:    func(string) error { calls = append(calls, "records"); return nil },
		Dropped:                 func(store.Thread, []string) { calls = append(calls, "dropped") },
		Forget:                  func(string) { calls = append(calls, "forget") },
	}
	if err := service.TearDown(thread.ID, ports); err != nil {
		t.Fatalf("TearDown: %v", err)
	}
	if _, err := database.GetThread(thread.ID); err != nil {
		t.Fatalf("the row after the teardown: %v, want it kept for the drop", err)
	}
	calls = append(calls, "|")
	if err := service.Drop(thread.ID, ports); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	want := []string{
		"background", "session", "setup", "shell", "terminal", "browser", "reconnect",
		// The second stop is under the mutation lock a command registers with.
		"shell", "attachments", "replay", "records", "|",
		"dropped", "terminal", "browser", "forget",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("cleanup order = %v, want %v", calls, want)
	}
	if _, err := database.GetThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread error = %v, want sql.ErrNoRows", err)
	}
	// A row already gone is nothing to do: a destroy the boot resumes.
	if err := service.Drop(thread.ID, ports); err != nil {
		t.Fatalf("Drop of a dropped row: %v", err)
	}
}

func TestTearDownContinuesCleanupButKeepsTheRowOnFailure(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	commitDelete(t, database, thread.ID)
	var calls []string
	cleanupErr := errors.New("disk busy")
	browserErr := errors.New("profile busy")
	recordsErr := errors.New("records busy")
	err = service.TearDown(thread.ID, DeletePorts{
		StopSession:       func(string) error { calls = append(calls, "session"); return nil },
		CloseBrowserPages: func(string) error { calls = append(calls, "browser"); return browserErr },
		CleanupAttachments: func(string) error {
			calls = append(calls, "attachments")
			return cleanupErr
		},
		CleanupReplayLog: func(string) error { calls = append(calls, "replay"); return nil },
		CleanupMemoryRecords: func(string) error {
			calls = append(calls, "records")
			return recordsErr
		},
		Forget: func(string) { calls = append(calls, "forget") },
	})
	if !errors.Is(err, cleanupErr) || !errors.Is(err, browserErr) || !errors.Is(err, recordsErr) {
		t.Fatalf("TearDown error = %v, want disk, browser and records errors", err)
	}
	if !slices.Equal(calls, []string{"session", "browser", "attachments", "replay", "records"}) {
		t.Fatalf("cleanup calls = %v", calls)
	}
	if _, err := database.GetThread(thread.ID); err != nil {
		t.Fatalf("row deleted after failed cleanup: %v", err)
	}
	if pending, err := database.ListPendingThreadDeletes(); err != nil || !slices.Equal(pending, []string{thread.ID}) {
		t.Fatalf("pending deletes = %v, %v; want the thread committed for the next attempt", pending, err)
	}
}

// An assistant home the delete may not remove refuses the teardown before
// anything of the thread stops.
func TestTearDownRefusesAHomeBeforeStoppingAnything(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	commitDelete(t, database, thread.ID)
	homeErr := errors.New("this assistant was not deleted: its home is not its own folder")
	stopped := false
	err = service.TearDown(thread.ID, DeletePorts{
		AssistantHome:  func(string) (func() error, error) { return nil, homeErr },
		StopRemoteWork: func(string) error { stopped = true; return nil },
		StopSession:    func(string) error { stopped = true; return nil },
	})
	if !errors.Is(err, homeErr) {
		t.Fatalf("TearDown error = %v, want the home's refusal", err)
	}
	if stopped {
		t.Fatal("a refused teardown stopped the thread's work")
	}
}

// The second close catches a page a companion action opened while the row
// was present. Its failure is reported, and the drop it follows stands.
func TestDropClosesBrowserPagesAgainAfterTheRowDrops(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	commitDelete(t, database, thread.ID)
	var rowPresent []bool
	var forgotten, dropped bool
	closeErr := errors.New("profile busy")
	err = destroy(service, thread.ID, DeletePorts{
		CloseBrowserPages: func(id string) error {
			_, err := database.GetThread(id)
			rowPresent = append(rowPresent, err == nil)
			if len(rowPresent) == 2 {
				return closeErr
			}
			return nil
		},
		Forget:  func(string) { forgotten = true },
		Dropped: func(store.Thread, []string) { dropped = true },
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("destroy error = %v, want the second close's error", err)
	}
	if !slices.Equal(rowPresent, []bool{true, false}) {
		t.Fatalf("browser closes saw the row present = %v, want before and after the drop", rowPresent)
	}
	if !forgotten || !dropped {
		t.Fatalf("forgotten = %v, dropped = %v after the row dropped, want both", forgotten, dropped)
	}
	if _, err := database.GetThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread error = %v, want sql.ErrNoRows", err)
	}
}

// The second close catches a terminal a client opened while the row was
// present. Its failure is reported, and the drop it follows stands.
func TestDropClosesTerminalsAgainAfterTheRowDrops(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	commitDelete(t, database, thread.ID)
	var rowPresent []bool
	var forgotten, dropped bool
	closeErr := errors.New("pty busy")
	err = destroy(service, thread.ID, DeletePorts{
		CloseTerminals: func(id string) error {
			_, err := database.GetThread(id)
			rowPresent = append(rowPresent, err == nil)
			if len(rowPresent) == 2 {
				return closeErr
			}
			return nil
		},
		Forget:  func(string) { forgotten = true },
		Dropped: func(store.Thread, []string) { dropped = true },
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("destroy error = %v, want the second close's error", err)
	}
	if !slices.Equal(rowPresent, []bool{true, false}) {
		t.Fatalf("terminal closes saw the row present = %v, want before and after the drop", rowPresent)
	}
	if !forgotten || !dropped {
		t.Fatalf("forgotten = %v, dropped = %v after the row dropped, want both", forgotten, dropped)
	}
	if _, err := database.GetThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread error = %v, want sql.ErrNoRows", err)
	}
}

func TestTearDownStopsRemoteWorkAdmittedDuringCleanup(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	commitDelete(t, database, thread.ID)
	admitted := false
	busy := errors.New("remote command admitted during cleanup could not be stopped")
	err = service.TearDown(thread.ID, DeletePorts{
		StopRemoteWork: func(string) error {
			if admitted {
				return busy
			}
			return nil
		},
		StopSession: func(string) error { admitted = true; return nil },
		CleanupAttachments: func(string) error {
			t.Fatal("removed files belonging to newly admitted remote work")
			return nil
		},
	})
	if !errors.Is(err, busy) {
		t.Fatalf("teardown error: %v", err)
	}
	if _, err := database.GetThread(thread.ID); err != nil {
		t.Fatalf("deleted source of newly admitted remote work: %v", err)
	}
}

// A thread a delete claimed is refused to every change (CheckMutable) and to
// a cleanup (CheckCleanup) until the delete commits or lets it go, and
// admitted to the delete's own check (CheckDelete). A committed thread is
// retired to a cleanup: it has nothing left for anyone else to clean.
func TestAClaimedThreadIsTheDeletesAlone(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimDeleteFamily("op", []string{thread.ID}, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.CheckMutable(thread.ID); !errors.Is(err, store.ErrThreadDeleting) {
		t.Fatalf("CheckMutable on a claimed thread = %v, want ErrThreadDeleting", err)
	}
	if _, err := service.CheckCleanup(thread.ID); !errors.Is(err, store.ErrThreadDeleting) {
		t.Fatalf("CheckCleanup on a claimed thread = %v, want ErrThreadDeleting", err)
	}
	for _, cleanup := range []bool{false, true} {
		if retired, err := service.CheckDelete(thread.ID, cleanup); err != nil || retired {
			t.Fatalf("CheckDelete(cleanup %v) on a claimed thread = %v, %v; want admitted", cleanup, retired, err)
		}
	}

	if err := database.ReleaseDeleteClaim("op"); err != nil {
		t.Fatal(err)
	}
	if err := service.CheckMutable(thread.ID); err != nil {
		t.Fatalf("CheckMutable once the claim was released: %v", err)
	}

	commitDelete(t, database, thread.ID)
	if retired, err := service.CheckCleanup(thread.ID); err != nil || !retired {
		t.Fatalf("CheckCleanup on a committed thread = %v, %v; want retired", retired, err)
	}
	if err := service.CheckMutable(thread.ID); !errors.Is(err, store.ErrThreadDeleting) {
		t.Fatalf("CheckMutable on a committed thread = %v, want ErrThreadDeleting", err)
	}
}

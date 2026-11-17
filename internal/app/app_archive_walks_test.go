package app

import (
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadtools"
)

// An archive is durable at its flip, and what it owns follows through a
// walk that runs after it (finishArchive). From the flip until the walk's
// end, the archive's remote archives are on their way (owesWorkerEnd):
// unfinished, as these tests name it.

// unfinishedArchives lists the archived threads with an archive walk under
// way: those whose remote workers' archives owesWorkerEnd counts as on
// their way.
func unfinishedArchives(t *testing.T, app *App) []string {
	t.Helper()
	app.archiveWalks.mu.Lock()
	var walking []string
	for id, walks := range app.archiveWalks.running {
		if walks > 0 {
			walking = append(walking, id)
		}
	}
	app.archiveWalks.mu.Unlock()
	var ids []string
	for _, id := range walking {
		row, err := app.store.GetThread(id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Archived {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func archivedIs(t *testing.T, app *App, step string, want map[string]bool) {
	t.Helper()
	for id, archived := range want {
		row, err := app.store.GetThread(id)
		if err != nil {
			t.Fatalf("%s: GetThread(%s): %v", step, id, err)
		}
		if row.Archived != archived {
			t.Errorf("%s: %s archived = %v, want %v", step, id, row.Archived, archived)
		}
	}
}

// Through either door, alone or with a title or a group, an archive is
// unfinished from its flip until its walk ends, each worker's from its own
// flip until its own walk ends; an unarchive ends it too, and the walk that
// follows leaves the reopened owner's workers open.
func TestAnArchiveIsUnfinishedUntilItsWalkEnds(t *testing.T) {
	t.Parallel()
	doors := map[string]func(t *testing.T, app *App) restingThreadWork{
		"ArchiveThread": func(t *testing.T, app *App) restingThreadWork {
			work, err := app.archiveThreadLocked("owner", "")
			if err != nil {
				t.Fatal(err)
			}
			return work
		},
		"thread_update": func(t *testing.T, app *App) restingThreadWork {
			_, work, err := app.applyThreadOrganizePatchLocked(t.Context(), "owner", threadapp.OrganizePatch{Archived: boolPtr(true)})
			if err != nil || work == nil {
				t.Fatalf("archive: %v, %v", work, err)
			}
			return *work
		},
		"thread_update with a title": func(t *testing.T, app *App) restingThreadWork {
			_, work, err := app.applyThreadOrganizePatchLocked(t.Context(), "owner",
				threadapp.OrganizePatch{Archived: boolPtr(true), Title: stringPtr("Renamed")})
			if err != nil || work == nil {
				t.Fatalf("archive: %v, %v", work, err)
			}
			return *work
		},
		"thread_update with a group": func(t *testing.T, app *App) restingThreadWork {
			_, work, err := app.applyThreadOrganizePatchLocked(t.Context(), "owner",
				threadapp.OrganizePatch{Archived: boolPtr(true), Group: stringPtr("Release")})
			if err != nil || work == nil {
				t.Fatalf("archive: %v, %v", work, err)
			}
			return *work
		},
	}
	for name, archive := range doors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newOrganizeFixture(t)
			for _, id := range []string{"owner", "worker", "deep"} {
				f.thread(t, id)
			}
			recordSpawn(t, f.app, "tok-worker", "owner", "worker")
			recordSpawn(t, f.app, "tok-deep", "worker", "deep")

			work := archive(t, f.app)
			if left := unfinishedArchives(t, f.app); !slices.Equal(left, []string{"owner"}) {
				t.Fatalf("unfinished after the flip = %v, want [owner]", left)
			}
			if outcome := f.app.finishArchive(work, reachLater); outcome.cleanup != nil || len(outcome.workers) > 0 {
				t.Fatalf("walk = %+v", outcome)
			}
			archivedIs(t, f.app, "after the walk", map[string]bool{"owner": true, "worker": true, "deep": true})
			if left := unfinishedArchives(t, f.app); len(left) != 0 {
				t.Fatalf("unfinished after the walk = %v", left)
			}

			// Reopened through thread_update, alone or with a title, while a
			// repeated archive's walk is on its way, a thread has no archive
			// left to finish, and that late walk leaves its workers open.
			if _, err := f.app.UnarchiveThread("deep"); err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct {
				id    string
				patch threadapp.OrganizePatch
			}{
				{"worker", threadapp.OrganizePatch{Archived: boolPtr(false), Title: stringPtr("Reopened")}},
				{"owner", threadapp.OrganizePatch{Archived: boolPtr(false)}},
			} {
				work, err := f.app.archiveThreadLocked(step.id, "")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := f.app.applyThreadOrganizePatchLocked(t.Context(), step.id, step.patch); err != nil {
					t.Fatal(err)
				}
				if left := unfinishedArchives(t, f.app); slices.Contains(left, step.id) {
					t.Fatalf("%s is unfinished after its unarchive: %v", step.id, left)
				}
				if outcome := f.app.finishArchive(work, reachLater); outcome.cleanup != nil || len(outcome.workers) > 0 {
					t.Fatalf("late walk of %s = %+v", step.id, outcome)
				}
			}
			archivedIs(t, f.app, "after the late walks", map[string]bool{"owner": false, "worker": false, "deep": false})
			if left := unfinishedArchives(t, f.app); len(left) != 0 {
				t.Fatalf("unfinished after the unarchives = %v", left)
			}
		})
	}
}

// A walk that ends while a newer archive of the same thread still has its
// walk to run leaves the archive unfinished: only the last walk to end
// ends it.
func TestOnlyTheLastWalkEndsAnArchive(t *testing.T) {
	t.Parallel()
	doors := map[string]func(t *testing.T, app *App, id string) restingThreadWork{
		"ArchiveThread": func(t *testing.T, app *App, id string) restingThreadWork {
			work, err := app.archiveThreadLocked(id, "")
			if err != nil {
				t.Fatal(err)
			}
			return work
		},
		"thread_update": func(t *testing.T, app *App, id string) restingThreadWork {
			_, work, err := app.applyThreadOrganizePatchLocked(t.Context(), id, threadapp.OrganizePatch{Archived: boolPtr(true)})
			if err != nil || work == nil {
				t.Fatalf("archive: %v, %v", work, err)
			}
			return *work
		},
	}
	for staleName, archiveStale := range doors {
		for currentName, archiveCurrent := range doors {
			t.Run(staleName+" then "+currentName, func(t *testing.T) {
				t.Parallel()
				f := newOrganizeFixture(t)
				owner := f.thread(t, "owner")
				before := f.thread(t, "before")
				recordSpawn(t, f.app, "tok-before", owner.ID, before.ID)
				stale := archiveStale(t, f.app, owner.ID)
				if _, err := f.app.UnarchiveThread(owner.ID); err != nil {
					t.Fatal(err)
				}
				after := f.thread(t, "after")
				recordSpawn(t, f.app, "tok-after", owner.ID, after.ID)
				current := archiveCurrent(t, f.app, owner.ID)
				f.app.finishArchive(stale, reachLater)
				if left := unfinishedArchives(t, f.app); !slices.Equal(left, []string{owner.ID}) {
					t.Fatalf("unfinished after the stale walk = %v, want the owner's newer archive", left)
				}
				f.app.finishArchive(current, reachLater)
				if left := unfinishedArchives(t, f.app); len(left) != 0 {
					t.Fatalf("unfinished after the current walk = %v", left)
				}
				archivedIs(t, f.app, "after both walks", map[string]bool{owner.ID: true, before.ID: true, after.ID: true})
			})
		}
	}
}

// The thread_update door reports each worker it could not archive beside
// the owner's archive, and the archive still ends: archiving again retries
// the worker.
func TestAWalkThatCouldNotArchiveAWorkerStillEnds(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	f.thread(t, "owner")
	f.thread(t, "stuck", func(row *store.Thread) { row.ForkPreparing = true })
	recordSpawn(t, f.app, "tok-stuck", "owner", "stuck")
	report, err := f.adapter.UpdateThreads(t.Context(), caller("someone"),
		threadtools.UpdateCall{ThreadIDs: []string{"owner"}, Archived: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if row := report.Results[0]; !row.Updated || len(row.WorkersNotArchived) != 1 {
		t.Fatalf("result = %+v, want the stuck worker reported", row)
	}
	if left := unfinishedArchives(t, f.app); len(left) != 0 {
		t.Fatalf("unfinished after a walk that reported its worker = %v", left)
	}
}

// A thread reopened and archived again while its older walk ends keeps
// the newer archive unfinished: the end of the older walk and a new
// archive's mark are ordered by the thread's action lock, so the end never
// clears a mark it did not count.
func TestAnArchiveMadeAsAnOlderWalkEndsStaysUnfinished(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	owner := f.thread(t, "owner")
	work, err := f.app.archiveThreadLocked(owner.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	rearchived := make(chan struct{})
	var once sync.Once
	f.app.archiveWalkEndingForTest = func(id string) {
		if id != owner.ID {
			return
		}
		once.Do(func() {
			go func() {
				defer close(rearchived)
				if _, err := f.app.UnarchiveThread(owner.ID); err != nil {
					t.Error(err)
					return
				}
				if _, err := f.app.archiveThreadLocked(owner.ID, ""); err != nil {
					t.Error(err)
				}
			}()
			// Room for the reopen and the archive to land inside the end.
			select {
			case <-rearchived:
			case <-time.After(500 * time.Millisecond):
			}
		})
	}
	f.app.finishArchive(work, reachLater)
	within(t, "the second archive", rearchived)
	archivedIs(t, f.app, "after the second archive", map[string]bool{owner.ID: true})
	if left := unfinishedArchives(t, f.app); !slices.Equal(left, []string{owner.ID}) {
		t.Fatalf("unfinished = %v, want the second archive, whose walk has not ended", left)
	}
}

// The archive of a worker's spawner on another computer is on its way from
// the spawner's flip, through its walk's end, until the ledger holds it: the
// walk stops counting only after it owed the archive. A spawner reopened
// while its walk runs, and a supersede, owe nothing.
func TestAWalkKeepsItsRemoteArchivesOnTheirWay(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	owner := f.thread(t, "owner")
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "tok-remote", CallerThreadID: owner.ID, Kind: store.ThreadRequestSpawn,
		TargetComputerID: "pc-there", TargetThreadID: "worker-there", State: store.ThreadRequestFinished,
		CreatedAt: 1, UpdatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	owes := func(want bool, step string) {
		t.Helper()
		if got, err := f.app.owesWorkerEnd("tok-remote"); err != nil || got != want {
			t.Fatalf("%s: owesWorkerEnd = %v, %v; want %v", step, got, err, want)
		}
	}
	owes(false, "before the archive")
	work, err := f.app.archiveThreadLocked(owner.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	owes(true, "after the flip")
	ended := false
	f.app.archiveWalkEndingForTest = func(id string) {
		if id == owner.ID {
			ended = true
			owes(true, "as the walk ends, before it owes")
		}
	}
	f.app.finishArchive(work, reachLater)
	if !ended {
		t.Fatal("the walk's end never ran")
	}
	f.app.archiveWalkEndingForTest = nil
	if walking := f.app.archiveWalking(owner.ID); walking {
		t.Fatal("the walk is still counted after it ended")
	}
	owes(true, "with the archive owed")

	if _, err := f.app.UnarchiveThread(owner.ID); err != nil {
		t.Fatal(err)
	}
	owes(false, "with a supersede owed")
	work, err = f.app.archiveThreadLocked(owner.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.UnarchiveThread(owner.ID); err != nil {
		t.Fatal(err)
	}
	owes(false, "reopened while its walk runs")
	f.app.finishArchive(work, reachLater)
	owes(false, "after the late walk of a reopened spawner")
}

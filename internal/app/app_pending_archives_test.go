package app

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// An archive is durable at its flip, and what it owns follows through a
// walk that runs after it (finishArchive). The flip marks the archive
// unfinished in its own transaction (archive_cascade), the walk's end
// clears the mark, and a boot finishes every archive a crash stopped
// before its walk ended (startPendingArchives).

// unfinishedArchives lists the threads whose archive a boot would finish.
func unfinishedArchives(t *testing.T, app *App) []string {
	t.Helper()
	ids, err := app.store.ListUnfinishedArchives()
	if err != nil {
		t.Fatal(err)
	}
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

// archiveCrash is a computer whose owner owns a worker that owns a
// worker, every one open, on a database a second boot reopens.
type archiveCrash struct {
	app  *App
	path string
}

func newArchiveCrash(t *testing.T) archiveCrash {
	t.Helper()
	_, path := newTestAppWithStorePath(t)
	return newArchiveCrashAt(t, path)
}

// newArchiveCrashAt is newArchiveCrash on the migrated database at path.
func newArchiveCrashAt(t *testing.T, path string) archiveCrash {
	t.Helper()
	app := newTestAppAtStorePath(t, path)
	project := store.Project{ID: "archive-project", Path: t.TempDir(), Name: "Archive", CreatedAt: 1, UpdatedAt: 1}
	if _, err := app.store.CreateProject(project); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owner", "worker", "deep"} {
		if err := app.store.CreateThread(store.Thread{
			ID: id, ProjectID: project.ID, Title: "Thread " + id, Provider: string(provider.Claude), Model: "claude-opus-4-7",
			Mode: threadmode.ModeChat, WorkspacePath: project.Path, RuntimeMode: string(provider.RuntimeApprovalRequired),
			CreatedAt: 1000, UpdatedAt: 1000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	recordSpawn(t, app, "tok-worker", "owner", "worker")
	recordSpawn(t, app, "tok-deep", "worker", "deep")
	return archiveCrash{app: app, path: path}
}

// reboot closes the crashed computer's database and opens it again as a
// boot does, running the boot's archive resume to its end.
func (c archiveCrash) reboot(t *testing.T) *App {
	t.Helper()
	c.app.appCancel()
	if err := c.app.store.Close(); err != nil {
		t.Fatal(err)
	}
	boot := newTestAppAtStorePath(t, c.path)
	boot.maintenance.firstReadsFallback = time.Millisecond
	boot.startPendingArchives()
	boot.waitPendingArchives()
	return boot
}

// An archive a crash cut after its flip, before its walk ended, is finished
// by the next boot through each door that archives: the owner's workers at
// every depth are archived and no archive is left unfinished. A repeated
// archive of an archived owner whose worker was reopened is finished the
// same way, and an owner reopened before the crash leaves its workers open.
func TestABootFinishesAnArchiveACrashCut(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		crash func(t *testing.T, app *App)
		want  map[string]bool
	}{
		"ArchiveThread": {
			crash: func(t *testing.T, app *App) {
				if _, err := app.archiveThreadLocked("owner", ""); err != nil {
					t.Fatal(err)
				}
			},
			want: map[string]bool{"owner": true, "worker": true, "deep": true},
		},
		"thread_update with a title": {
			crash: func(t *testing.T, app *App) {
				patch := threadapp.OrganizePatch{Archived: boolPtr(true), Title: stringPtr("Archived owner")}
				if _, _, err := app.applyThreadOrganizePatchLocked(t.Context(), "owner", patch); err != nil {
					t.Fatal(err)
				}
			},
			want: map[string]bool{"owner": true, "worker": true, "deep": true},
		},
		"an archived owner archived again": {
			crash: func(t *testing.T, app *App) {
				if _, err := app.ArchiveThread("owner"); err != nil {
					t.Fatal(err)
				}
				if _, err := app.UnarchiveThread("deep"); err != nil {
					t.Fatal(err)
				}
				if _, err := app.archiveThreadLocked("owner", ""); err != nil {
					t.Fatal(err)
				}
			},
			want: map[string]bool{"owner": true, "worker": true, "deep": true},
		},
		"an owner reopened before the crash": {
			crash: func(t *testing.T, app *App) {
				if _, err := app.archiveThreadLocked("owner", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := app.UnarchiveThread("owner"); err != nil {
					t.Fatal(err)
				}
			},
			want: map[string]bool{"owner": false, "worker": false, "deep": false},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			crash := newArchiveCrash(t)
			tc.crash(t, crash.app)
			boot := crash.reboot(t)
			archivedIs(t, boot, "after the boot", tc.want)
			if left := unfinishedArchives(t, boot); len(left) != 0 {
				t.Errorf("unfinished archives after the boot = %v", left)
			}
		})
	}
}

// The boot's own start resumes an unfinished archive, with no client
// attached and nothing running.
func TestTheBootStartResumesUnfinishedArchives(t *testing.T) {
	a := newBootTestApp(t)
	path := filepath.Join(a.dataDirOverride, "agent-overflow", databaseFileName)
	migratedDatabaseAt(t, path)
	crash := newArchiveCrashAt(t, path)
	if _, err := crash.app.archiveThreadLocked("owner", ""); err != nil {
		t.Fatal(err)
	}
	crash.app.appCancel()
	if err := crash.app.store.Close(); err != nil {
		t.Fatal(err)
	}

	a.maintenance.firstReadsFallback = time.Millisecond
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntil(t, 30*time.Second, func() bool {
		ids, err := a.store.ListUnfinishedArchives()
		return err == nil && len(ids) == 0
	})
	archivedIs(t, a, "after the boot", map[string]bool{"owner": true, "worker": true, "deep": true})
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
			if outcome := f.app.finishArchive(work); outcome.cleanup != nil || len(outcome.workers) > 0 {
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
				if outcome := f.app.finishArchive(work); outcome.cleanup != nil || len(outcome.workers) > 0 {
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
// ends it, so a crash in between still finishes the newer one.
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
				f.app.finishArchive(stale)
				if left := unfinishedArchives(t, f.app); !slices.Equal(left, []string{owner.ID}) {
					t.Fatalf("unfinished after the stale walk = %v, want the owner's newer archive", left)
				}
				f.app.finishArchive(current)
				if left := unfinishedArchives(t, f.app); len(left) != 0 {
					t.Fatalf("unfinished after the current walk = %v", left)
				}
				archivedIs(t, f.app, "after both walks", map[string]bool{owner.ID: true, before.ID: true, after.ID: true})
			})
		}
	}
}

// An assistant's handoff archives its old thread, whose spawns moved to the
// successor, so its archive has nothing to walk and ends with the handoff.
func TestAHandOffLeavesNoUnfinishedArchive(t *testing.T) {
	t.Parallel()
	a, old := remoteJobAssistant(t)
	successor, err := a.nextAssistantThread(t.Context(), old.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	a.waitAgentNoticeWork()
	archivedIs(t, a, "after the handoff", map[string]bool{old.ID: true, successor.ID: false})
	if left := unfinishedArchives(t, a); len(left) != 0 {
		t.Fatalf("unfinished after the handoff = %v", left)
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

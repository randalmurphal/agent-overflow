package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func TestForkRejectsSourceFlushFailure(t *testing.T) {
	for _, message := range []bool{false, true} {
		t.Run(map[bool]string{false: "tail", true: "message"}[message], func(t *testing.T) {
			a, path := newTestAppWithStorePath(t)
			fixture := newMidTurnForkFixture(t, "mid-turn-session", midTurnSourceJSONL)
			source := createAppTestThread(t, a, "flush-source", "claude", fixture.workspace)
			source.SessionRef = fixture.sessionID
			if err := a.store.UpdateThread(source); err != nil {
				t.Fatal(err)
			}
			seedMidTurnSourceRows(t, a.store, source.ID)
			a.triage = triage.NewRouter(a.store, func(eventchan.Channel, any) {})
			t.Cleanup(func() {
				if err := a.triage.Wait(context.Background()); err != nil {
					t.Error(err)
				}
			})
			if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: source.ID, ItemID: "flush-text", Content: "initial", Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := a.triage.FlushThread(source.ID); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := raw.Exec(`DROP TRIGGER reject_fork_flush`); err != nil {
					t.Error(err)
				}
				if err := raw.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := raw.Exec(`CREATE TRIGGER reject_fork_flush BEFORE UPDATE OF summary ON items WHEN NEW.thread_id='flush-source' BEGIN SELECT RAISE(ABORT,'injected fork flush failure'); END`); err != nil {
				t.Fatal(err)
			}
			// Install the fault before starting this flush window. Fixture I/O
			// must not give the background timer a head start over the fork.
			if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: source.ID, ItemID: "flush-text", Content: " buffered", Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if message {
				_, err = a.ForkThreadFromMessage(t.Context(), source.ID, "src-u1")
			} else {
				_, err = a.ForkThread(t.Context(), source.ID, nil)
			}
			if err == nil || !strings.Contains(err.Error(), "injected fork flush failure") {
				t.Fatalf("fork must surface the source flush failure: %v", err)
			}
			threads, err := a.store.ListThreadsWithItems()
			if err != nil {
				t.Fatal(err)
			}
			for _, thread := range threads {
				if thread.ForkedFromThreadID == source.ID {
					t.Fatalf("failed source flush published fork %s", thread.ID)
				}
			}
		})
	}
}

func TestForkPreparationPublication(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "failure"}[fail], func(t *testing.T) {
			a := newTestApp(t)
			fixture := newMidTurnForkFixture(t, "preparation", `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"preparation","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"preparation","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}
`)
			source := createAppTestThread(t, a, "source", "claude", fixture.workspace)
			if !fail {
				source.SessionRef = fixture.sessionID
				if err := a.store.UpdateThread(source); err != nil {
					t.Fatal(err)
				}
			}
			insertUserItemWithMeta(t, a.store, source.ID, "u0", 0, "first", `{"provider_item_id":"u0"}`)
			var pendingID string
			ready, deleted := false, false
			a.testEmitHook = func(name string, data any) {
				event, ok := data.(triage.ThreadUpdateEvent)
				if !ok {
					return
				}
				if event.Action == triage.ThreadActionDeleted {
					if event.ID != pendingID {
						t.Errorf("deleted=%s pending=%s", event.ID, pendingID)
					}
					deleted = true
					return
				}
				if event.Thread == nil || event.Thread.ForkedFromThreadID != source.ID {
					return
				}
				row := *event.Thread
				if row.ForkPreparing {
					pendingID = row.ID
					listed, err := a.store.ListThreadsWithItems()
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, thread := range listed {
						if thread.ID == row.ID {
							found = true
						}
					}
					if !found {
						t.Error("pending fork absent from catalog before clone")
					}
					for name, read := range map[string]func() error{
						"page": func() error {
							_, err := a.ListThreadSliceAround(context.Background(), row.ID, "", 25, TimelinePageOptions{PageShape: PageShape{}})
							return err
						},
						"older": func() error {
							_, err := a.ListItemsBeforeCursor(context.Background(), row.ID, store.TimelineCursor{}, 25, TimelinePageOptions{PageShape: PageShape{}})
							return err
						},
						"newer": func() error {
							_, err := a.ListItemsAfterCursor(context.Background(), row.ID, store.TimelineCursor{}, 25, TimelinePageOptions{PageShape: PageShape{}})
							return err
						},
						"subagent":   func() error { _, err := a.ListSubagentDescendants(row.ID, "root", true); return err },
						"plans":      func() error { _, err := a.ListThreadProposedPlans(row.ID); return err },
						"background": func() error { _, err := a.ListLiveBackgroundTasks(row.ID); return err },
						"ticks":      func() error { _, err := a.GetThreadUserMessageTicks(row.ID); return err },
						"history":    func() error { _, err := a.GetThreadUserMessageHistory(row.ID, 10); return err },
						"preview":    func() error { _, err := a.GetThreadTurnPreview(row.ID, "item"); return err },
						"item":       func() error { _, err := a.GetThreadItem(row.ID, "item"); return err },
						"projection": func() error { _, err := a.GetThreadItemProjectionSource(row.ID, "item"); return err },
						"turns":      func() error { _, err := a.ListRecentTurns(row.ID, 10); return err },

						"items": func() error { _, err := a.ListItems(row.ID, true); return err },
						"sync": func() error {
							_, err := a.SyncThreadWindow(context.Background(), row.ID, SyncThreadWindowRequest{})
							return err
						},
						"payload": func() error { _, err := a.GetPayloadData(row.ID, "payload"); return err },
						"runs": func() error {
							_, err := a.ListActivityRunMembers(context.Background(), row.ID, ActivityRunMembersRequest{})
							return err
						},
						"edits":    func() error { _, err := a.ListThreadEditDiffs(row.ID); return err },
						"mutation": func() error { return a.threadApplication().CheckMutable(row.ID) },
					} {
						if err := read(); !errors.Is(err, store.ErrForkPreparing) {
							t.Errorf("%s accepted pending fork: %v", name, err)
						}
					}
				} else {
					ready = true
					if row.ID != pendingID {
						t.Error("ready publication changed identity")
					}
					persisted, err := a.store.GetThread(row.ID)
					if err != nil || persisted.ForkPreparing || persisted.PendingForkRef != row.PendingForkRef || persisted.SessionRef != row.SessionRef {
						t.Errorf("ready state not durable: %+v %v", persisted, err)
					}
					items, err := a.ListItems(row.ID, true)
					items = withoutForkDividers(items)
					if err != nil || len(items) != 1 || items[0].Summary != "first" {
						t.Errorf("published history=%+v: %v", items, err)
					}
				}
			}
			var at *int
			if fail {
				insertUserItemWithMeta(t, a.store, source.ID, "u1", 1, "second", `{}`)
				index := 0
				at = &index
			}
			fork, err := a.ForkThread(t.Context(), source.ID, at)
			if fail {
				if err == nil || !deleted || ready {
					t.Fatalf("failure cleanup: err=%v deleted=%v ready=%v", err, deleted, ready)
				}
			} else {
				if err != nil || !ready || deleted || fork.ForkPreparing {
					t.Fatalf("publication: %+v err=%v ready=%v deleted=%v", fork, err, ready, deleted)
				}
			}
			if pendingID == "" {
				t.Fatal("no preparation event")
			}
		})
	}
}

func TestForkPreparationBootCleanup(t *testing.T) {
	a := newTestApp(t)
	source := createAppTestThread(t, a, "source", "claude", t.TempDir())
	fork := store.BuildForkedThread(source)
	fork.ForkPreparing = true
	if err := a.store.CreateThread(fork); err != nil {
		t.Fatal(err)
	}
	stale := fork
	stale.ForkPreparing = false
	stale.Title = "stale update"
	if err := a.store.UpdateThread(stale); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CheckForkReady(fork.ID); !errors.Is(err, store.ErrForkPreparing) {
		t.Fatalf("whole-row update cleared readiness: %v", err)
	}
	if err := a.cleanupPreparingForks(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.GetThread(fork.ID); err == nil {
		t.Fatal("interrupted fork survived startup cleanup")
	}
	if _, err := a.store.GetThread(source.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.cleanupPreparingForks(); err != nil {
		t.Fatal(err)
	}
}

func TestIdleClaudeForkKeepsItsProviderCutAfterSourceAdvances(t *testing.T) {
	a := newTestApp(t)
	fixture := newMidTurnForkFixture(t, "idle-cut", `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"idle-cut","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"idle-cut","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}
`)
	source := createAppTestThread(t, a, "source", "claude", fixture.workspace)
	source.SessionRef = fixture.sessionID
	if err := a.store.UpdateThread(source); err != nil {
		t.Fatal(err)
	}
	insertUserItemWithMeta(t, a.store, source.ID, "u0", 0, "first", `{"provider_item_id":"u0"}`)
	fork, err := a.ForkThread(t.Context(), source.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fork.PendingForkResumeAt != "a0" {
		t.Fatalf("idle fork not pinned: %+v", fork)
	}
	file, err := os.OpenFile(fixture.jsonlPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(`{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"idle-cut","message":{"role":"user","content":"later"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"idle-cut","message":{"role":"assistant","content":[{"type":"text","text":"later answer"}]}}
`)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	insertUserItemWithMeta(t, a.store, source.ID, "u1", 1, "later", `{"provider_item_id":"u1"}`)
	second, err := a.ForkThread(t.Context(), fork.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []store.Thread{fork, second} {
		if row.PendingForkRef != source.SessionRef || row.PendingForkResumeAt != "a0" {
			t.Fatalf("cut drifted: %+v", row)
		}
		resumeAt, err := resolveClaudeForkResumeAt(testProviderProjectsDir(t), row.PendingForkRef, row.WorkspacePath, row.PendingForkResumeAt)
		if err != nil || resumeAt != "a0" {
			t.Fatalf("provider resume cut=%q: %v", resumeAt, err)
		}
		items, err := forkConversationItems(a.store, row.ID)
		if err != nil || len(items) != 1 || items[0].Summary != "first" {
			t.Fatalf("displayed cut drifted: %+v: %v", items, err)
		}
	}
}

func TestForkPreparationCancellationRemovesPendingRow(t *testing.T) {
	a := newTestApp(t)
	source := createAppTestThread(t, a, "source", "codex", t.TempDir())
	insertUserItemWithMeta(t, a.store, source.ID, "u0", 0, "first", `{}`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var pending string
	deleted := false
	a.testEmitHook = func(name string, data any) {
		event, ok := data.(triage.ThreadUpdateEvent)
		if !ok {
			return
		}
		if event.Thread != nil && event.Thread.ForkPreparing {
			pending = event.Thread.ID
			cancel()
		}
		if event.Action == triage.ThreadActionDeleted && event.ID == pending {
			deleted = true
		}
	}
	// The first-message path needs no native session. Cancellation must clean
	// up before it publishes a usable empty fork and restored prompt draft.
	_, err := a.ForkThreadFromMessage(ctx, source.ID, "u0")
	if !errors.Is(err, context.Canceled) || pending == "" || !deleted {
		t.Fatalf("err=%v pending=%s deleted=%v", err, pending, deleted)
	}
	if _, err := a.store.GetThread(pending); err == nil {
		t.Fatal("canceled fork retained")
	}
}

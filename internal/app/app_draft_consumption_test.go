package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The predicate uses all persisted fields, not expanded provider text or a
// timestamp. Matching and clearing share SaveDraft's encoding/normalization.
func TestDraftConsumptionMatchesPersistedState(t *testing.T) {
	for name, change := range map[string]func(*DraftSnapshot){
		"content":     func(d *DraftSnapshot) { d.Content += " next" },
		"attachments": func(d *DraftSnapshot) { d.AttachmentIDs = []string{"new-upload"} },
		"terminal":    func(d *DraftSnapshot) { d.TerminalChips = []TerminalChip{{ID: "new-chip", Content: "new output"}} },
		"plan":        func(d *DraftSnapshot) { d.SourceProposedPlan = &SourceProposedPlan{ItemID: "new-plan"} },
	} {
		t.Run(name, func(t *testing.T) {
			app := draftTestApp(t, "draft-consume")
			old := DraftSnapshot{Content: "raw composer", AttachmentIDs: []string{"attachment"}, TerminalChips: []TerminalChip{{ID: "chip", Content: "output"}}, SourceProposedPlan: &SourceProposedPlan{ItemID: "plan"}}
			newer := old
			change(&newer)
			if err := app.SaveDraft(t.Context(), "draft-consume", newer.Content, newer.AttachmentIDs, newer.TerminalChips, newer.SourceProposedPlan); err != nil {
				t.Fatal(err)
			}
			broadcasts := captureDraftBroadcasts(t, app)
			if err := app.removeThreadDraft(transport.ClientIdentity{}, "draft-consume", &old); err != nil {
				t.Fatal(err)
			}
			broadcasts.expectSilence("nonmatching consumption")
			if _, found, err := app.store.GetThreadDraft("draft-consume"); err != nil || !found {
				t.Fatalf("newer draft lost: found=%v err=%v", found, err)
			}
			if err := app.removeThreadDraft(transport.ClientIdentity{}, "draft-consume", &newer); err != nil {
				t.Fatal(err)
			}
			broadcasts.expectOne("matching consumption")
			if _, found, err := app.store.GetThreadDraft("draft-consume"); err != nil || found {
				t.Fatalf("matching draft remains: found=%v err=%v", found, err)
			}
		})
	}
	t.Run("normalized empty fields", func(t *testing.T) {
		app := draftTestApp(t, "empty-fields")
		if err := app.SaveDraft(t.Context(), "empty-fields", "raw", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		snapshot := &DraftSnapshot{Content: "raw", AttachmentIDs: []string{}, TerminalChips: []TerminalChip{}, SourceProposedPlan: &SourceProposedPlan{}}
		if err := app.removeThreadDraft(transport.ClientIdentity{}, "empty-fields", snapshot); err != nil {
			t.Fatal(err)
		}
		if _, found, err := app.store.GetThreadDraft("empty-fields"); err != nil || found {
			t.Fatalf("normalized draft not consumed: found=%v err=%v", found, err)
		}
	})
}

func TestComposerAdmissionConsumesOnlyCapturedDraft(t *testing.T) {
	for _, path := range []string{"direct", "queue", "busy-direct"} {
		for _, newer := range []bool{false, true} {
			name := "matching"
			if newer {
				name = "newer"
			}
			t.Run(path+"/"+name, func(t *testing.T) {
				app, _ := newAppForFlushQueueRPC(t)
				thread := testThread("draft-admission")
				thread.WorkspacePath = t.TempDir()
				if err := app.store.CreateThread(thread); err != nil {
					t.Fatal(err)
				}
				if path == "direct" {
					sess, err := claude.NewSession(context.Background(), thread.ID, claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath}, func(provider.ProviderEvent) {})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = sess.Close() })
					app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "test", Claude: sess})
				} else if path == "busy-direct" {
					if err := app.store.InsertTurn(store.Turn{TurnID: "active", ThreadID: thread.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli()}); err != nil {
						t.Fatal(err)
					}
				}
				saved := "raw composer"
				if newer {
					saved = "next message from another device"
				}
				if err := app.SaveDraft(t.Context(), thread.ID, saved, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				opts := SendMessageOptions{SendID: "draft-send", ReconcileBySendID: true, ConsumeDraft: &DraftSnapshot{Content: "raw composer"}}
				// The submitted text can differ after chip/slash-command expansion.
				if path == "queue" {
					if _, err := app.RegisterQueueItem(t.Context(), thread.ID, "provider-expanded text", opts); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := app.SendMessageWithOptions(t.Context(), thread.ID, "provider-expanded text", opts); err != nil {
						t.Fatal(err)
					}
				}
				draft, found, err := app.store.GetThreadDraft(thread.ID)
				if err != nil || found != newer || (found && draft.Content != saved) {
					t.Fatalf("draft after %s: %+v found=%v err=%v", path, draft, found, err)
				}
				if path != "direct" {
					queued := app.triage.QueuedFlushItems(thread.ID)
					if len(queued) != 1 {
						t.Fatalf("queue = %+v", queued)
					}
					if strings.Contains(string(queued[0].Payload), "consumeDraft") || strings.Contains(string(queued[0].Payload), "raw composer") {
						t.Fatalf("consumed draft leaked into durable payload: %s", queued[0].Payload)
					}
				}
			})
		}
	}
}

func TestQueueDispatchPreservesDraftWrittenAfterAdmission(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("draft-after-queue")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.SaveDraft(t.Context(), thread.ID, "queued text", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Legacy admission still consumes the old draft; dispatch must never consume
	// another draft typed after admission, regardless of original client version.
	if _, err := app.RegisterQueueItem(t.Context(), thread.ID, "queued text", SendMessageOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := app.store.GetThreadDraft(thread.ID); err != nil || found {
		t.Fatalf("legacy admission did not consume: %v %v", found, err)
	}
	if err := app.SaveDraft(t.Context(), thread.ID, "next unsent message", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "test", Codex: sess})
	app.dispatchFlush(thread.ID, app.triage.QueuedFlushItems(thread.ID))
	state, err := app.GetThreadLiveState(thread.ID)
	if err != nil || len(state.FlushedItems) != 1 {
		t.Fatalf("queue did not dispatch: %+v %v", state, err)
	}
	draft, found, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !found || draft.Content != "next unsent message" {
		t.Fatalf("later draft lost at dispatch: %+v %v %v", draft, found, err)
	}
}

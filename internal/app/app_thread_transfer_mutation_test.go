package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transferwire"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"github.com/google/uuid"
)

func TestThreadTransferFencesUserMutationsUntilCopySeals(t *testing.T) {
	a := newTestAppWithStore(t)
	thread := testThread(uuid.NewString())
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.SaveDraft(ctx, thread.ID, "keep this draft", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	row, err := a.store.CreateThreadTransfer(store.ThreadTransfer{ID: uuid.NewString(), ThreadID: thread.ID,
		PeerBackendID: uuid.NewString(), Direction: "outgoing", Kind: "copy", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	operations := map[string]func() error{
		"draft save":         func() error { return a.SaveDraft(ctx, thread.ID, "overwrite", nil, nil, nil) },
		"draft clear":        func() error { return a.ClearDraft(ctx, thread.ID) },
		"delete":             func() error { return a.DeleteThread(thread.ID) },
		"empty draft delete": func() error { return mustErr(a.DeleteEmptyDraftThread(thread.ID)) },
		"archive":            func() error { return a.ArchiveThread(thread.ID) },
		"unarchive":          func() error { return mustErr(a.UnarchiveThread(thread.ID)) },
		"rename":             func() error { return a.RenameThread(thread.ID, "changed") },
		"model":              func() error { return mustErr(a.UpdateThreadModel(thread.ID, "changed")) },
		"effort":             func() error { return mustErr(a.UpdateThreadReasoningEffort(thread.ID, "high")) },
		"fast mode":          func() error { return mustErr(a.UpdateThreadFastMode(thread.ID, true)) },
		"mode":               func() error { return mustErr(a.UpdateThreadMode(thread.ID, "plan")) },
		"runtime":            func() error { return a.applyRuntimeMode(thread.ID, provider.RuntimeReadOnly) },
		"context":            func() error { return mustErr(a.UpdateThreadContextSettings(thread.ID, ContextSettingsUpdate{})) },
		"workspace":          func() error { return a.ensureThreadChangeAllowed(thread.ID) },
		"fork":               func() error { return mustErr(a.ForkThread(ctx, thread.ID, nil)) },
		"message fork":       func() error { return mustErr(a.ForkThreadFromMessage(ctx, thread.ID, "message")) },
		"queue":              func() error { return mustErr(a.RegisterQueueItem(ctx, thread.ID, "late prompt", SendMessageOptions{})) },
		"plan comment":       func() error { return mustErr(a.CreateProposedPlanComment(thread.ID, store.ProposedPlanCommentInput{})) },
		"diff comment":       func() error { return mustErr(a.CreateDiffReviewComment(thread.ID, store.DiffReviewCommentInput{})) },
		"group":              func() error { return mustErr(a.SetThreadGroup([]string{thread.ID}, "")) },
	}
	for name, action := range operations {
		t.Run(name, func(t *testing.T) {
			var pending *store.ThreadTransferError
			if err := action(); !errors.As(err, &pending) || pending.Moved || pending.OperationID != row.ID {
				t.Fatalf("missing transfer refusal: %v", err)
			}
		})
	}
	draft, err := a.GetDraft(thread.ID)
	if err != nil || draft.Content != "keep this draft" {
		t.Fatalf("draft changed: %+v %v", draft, err)
	}
	if _, err := a.store.BindThreadTransferArchive(row.ID, transferwire.Upload{SHA256: strings.Repeat("b", 64), Size: 1024}); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveDraft(ctx, thread.ID, "continue original while uploading", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.RenameThread(thread.ID, "original still usable"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}
}

func TestThreadTransferRefusesHistoryWithoutNativeResume(t *testing.T) {
	a := newTestAppWithStore(t)
	thread := testThread(uuid.NewString())
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := a.store.InsertItem(store.Item{ID: "message", ThreadID: thread.ID, Role: "user", Kind: "user_text", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := a.checkTransferIdle(thread); err == nil || !strings.Contains(err.Error(), "no saved provider session") {
		t.Fatalf("lost native history accepted: %v", err)
	}
}

// A recurring job never holds a transfer: the transfer ends the session
// and its jobs either way, so the dialog names the job instead. A one-shot
// job is a turn the session still owes, and it refuses the transfer.
func TestThreadTransferNamesRecurringJobsInsteadOfRefusing(t *testing.T) {
	a := newTestAppWithStore(t)
	a.triage = triage.NewRouter(a.store, func(eventchan.Channel, any) {})
	thread := testThread(uuid.NewString())
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	arm := func(jobs ...provider.ScheduledJob) {
		t.Helper()
		meta, err := json.Marshal(provider.ScheduledJobsMeta{Jobs: jobs})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventScheduledJobs, ThreadID: thread.ID, Meta: meta}); err != nil {
			t.Fatal(err)
		}
	}
	loop := provider.ScheduledJob{ID: "6ae146b0", Cron: "*/5 * * * *", Recurring: true, FireByUnixMs: time.Now().Add(10 * time.Minute).UnixMilli()}
	stale := provider.ScheduledJob{ID: "0dd0dd00", Cron: "0 9 * * *", Recurring: true, FireByUnixMs: time.Now().Add(-time.Hour).UnixMilli()}
	arm(loop, stale)
	if err := a.checkTransferIdle(thread); err != nil {
		t.Fatalf("a recurring job refused the transfer: %v", err)
	}
	jobs, err := a.ListThreadRecurringJobs(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []ThreadRecurringJob{{ID: "6ae146b0", Cron: "*/5 * * * *"}}; !reflect.DeepEqual(jobs, want) {
		t.Fatalf("ListThreadRecurringJobs = %+v, want %+v (a job past its deadline is no longer counted)", jobs, want)
	}

	arm(loop, provider.ScheduledJob{ID: "718ab791", Cron: "0 9 * * 1", FireByUnixMs: time.Now().Add(24 * time.Hour).UnixMilli()})
	if err := a.checkTransferIdle(thread); err == nil || !strings.Contains(err.Error(), "one-time scheduled jobs") {
		t.Fatalf("a pending one-shot job did not refuse the transfer: %v", err)
	}
}

func TestThreadTransferWorkspaceRequiresDestinationGitGrant(t *testing.T) {
	a := transferTestBackend(t)
	backendID, _ := a.backendIdentity()
	limited := pairSessionWithScopes(t, a, "transfer-limited", []identity.Scope{identity.ScopeThreadsOperate})
	intent := ThreadTransferIntent{OperationID: uuid.NewString(), SourceBackendID: uuid.NewString(), DestinationBackendID: backendID,
		SourceThreadID: uuid.NewString(), TargetThreadID: uuid.NewString(), Kind: "copy", Provider: "claude",
		RuntimeMode: string(provider.RuntimeApprovalRequired), IncludeWorkspace: true, ActivationHash: strings.Repeat("a", 64)}
	wantScopeRefusal(t, mustErr(a.CreateThreadTransferOffer(callFrom(limited.ID, false), intent, "missing-project", "", "")), transport.ScopeGitOperate)
	granted := pairSessionWithScopes(t, a, "transfer-granted", []identity.Scope{identity.ScopeThreadsOperate, identity.ScopeGitOperate})
	err := mustErr(a.CreateThreadTransferOffer(callFrom(granted.ID, false), intent, "missing-project", "", ""))
	if _, refused := transport.AuthzFrame(err); err == nil || refused {
		t.Fatalf("Git grant did not reach project validation: %v", err)
	}
}

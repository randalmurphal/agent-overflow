package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// discussionAuthzFixture is an identity-wired App whose participant
// sessions and turn prompts are recorded instead of run, plus a global
// two-participant definition and a parent thread in approval-required
// mode, which needs no autonomy itself.
type discussionAuthzFixture struct {
	app                *App
	granted, ungranted string
	parent             store.Thread
	definition         store.DiscussionDefinition
	mu                 sync.Mutex
	started            []string
	prompted           chan string
}

func newDiscussionAuthzFixture(t *testing.T) *discussionAuthzFixture {
	t.Helper()
	app, granted, ungranted := autonomyApp(t)
	f := &discussionAuthzFixture{app: app, granted: granted, ungranted: ungranted, prompted: make(chan string, 8)}
	app.startSessionFn = func(threadID string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.started = append(f.started, threadID)
		return nil
	}
	app.sendMessageFn = func(threadID, _ string, _ []string) error {
		f.prompted <- threadID
		return nil
	}
	f.parent = threadInMode(t, app, provider.RuntimeApprovalRequired)
	f.definition = store.DiscussionDefinition{
		ID: "def-authz", Name: "Reviewers", Scope: "global",
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 4},
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := app.store.CreateDiscussionDef(f.definition); err != nil {
		t.Fatalf("CreateDiscussionDef: %v", err)
	}
	return f
}

func (f *discussionAuthzFixture) startedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

// TestDiscussionsNeedAutonomyForTheThreadsTheyDrive. A discussion drives
// every participant: a post prompts the next speaker, and each finished turn
// prompts the one after it. The method floor admits a session holding
// threads:operate, so the question asked inside is the participants'
// resolved runtime mode, not the parent's: here the parent is
// approval-required and the participants a definition starts run
// full-access.
func TestDiscussionsNeedAutonomyForTheThreadsTheyDrive(t *testing.T) {
	t.Parallel()
	f := newDiscussionAuthzFixture(t)
	app := f.app
	operateOnly := []string{string(identity.ScopeThreadsRead), string(identity.ScopeThreadsOperate)}
	for _, method := range []string{"StartDiscussion", "StartDiscussionByID", "PostChannelMessage"} {
		if refusal := transport.AuthorizeSessionMethod(operateOnly, method, transport.CallerProof{}); refusal != nil {
			t.Fatalf("the %s floor refused threads:operate (%s); this test no longer reaches the method", method, refusal.Message)
		}
	}
	limited := callFrom(f.ungranted, false)

	wantScopeRefusal(t, app.StartDiscussion(limited, f.parent.ID, f.definition.Name), transport.ScopeThreadsAutonomy)
	wantScopeRefusal(t, app.StartDiscussionByID(limited, f.parent.ID, f.definition.ID), transport.ScopeThreadsAutonomy)
	children, err := app.store.ListChildThreads(f.parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads: %v", err)
	}
	if len(children) != 0 || len(f.startedSessions()) != 0 {
		t.Fatalf("a refused start left %d participants and started %v", len(children), f.startedSessions())
	}

	if err := app.StartDiscussion(callFrom(f.granted, false), f.parent.ID, f.definition.Name); err != nil {
		t.Fatalf("a session holding threads:autonomy was refused: %v", err)
	}
	parent, err := app.store.GetThread(f.parent.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	children, err = app.store.ListChildThreads(parent.ID)
	if err != nil || len(children) != 2 {
		t.Fatalf("ListChildThreads = %d, %v; want 2 participants", len(children), err)
	}
	for _, child := range children {
		if child.RuntimeMode != string(provider.RuntimeFullAccess) {
			t.Fatalf("participant %s runs %q; this test covers participants that need autonomy", child.ID, child.RuntimeMode)
		}
	}

	wantScopeRefusal(t, mustErr(app.PostChannelMessage(limited, parent.DiscussionID, "go")), transport.ScopeThreadsAutonomy)
	messages, err := app.store.ListChannelMessages(parent.DiscussionID, -1, 0)
	if err != nil {
		t.Fatalf("ListChannelMessages: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("a refused post was persisted: %+v", messages)
	}
	select {
	case threadID := <-f.prompted:
		t.Fatalf("a refused post prompted participant %s", threadID)
	case <-time.After(100 * time.Millisecond):
	}

	// One participant moved to approval-required is not enough: the other
	// still takes a turn later in the same discussion.
	if _, err := app.UpdateThreadRuntimeMode(context.Background(), children[0].ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatalf("UpdateThreadRuntimeMode: %v", err)
	}
	wantScopeRefusal(t, mustErr(app.PostChannelMessage(limited, parent.DiscussionID, "go")), transport.ScopeThreadsAutonomy)

	if _, err := app.UpdateThreadRuntimeMode(context.Background(), children[1].ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatalf("UpdateThreadRuntimeMode: %v", err)
	}
	if _, err := app.PostChannelMessage(limited, parent.DiscussionID, "go"); err != nil {
		t.Fatalf("posting into a discussion whose participants all need approval was refused: %v", err)
	}
	select {
	case <-f.prompted:
	case <-time.After(2 * time.Second):
		t.Fatal("the admitted post prompted no participant")
	}
}

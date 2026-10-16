package app

import (
	"slices"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// sessionLiveFrames returns the provider:session_live answers published for
// threadID, in order, once the publisher has nothing left to do.
func sessionLiveFrames(t *testing.T, app *App, rec *emitRecorder, threadID string) []bool {
	t.Helper()
	waitSendsPendingIdle(t, app)
	var out []bool
	for _, call := range rec.snapshot() {
		if call.Channel != string(eventchan.ProviderSessionLive) {
			continue
		}
		if evt := call.Data.(SessionLiveEvent); evt.ThreadID == threadID {
			out = append(out, evt.Live)
		}
	}
	return out
}

func assertSessionLiveFrames(t *testing.T, app *App, rec *emitRecorder, threadID, stage string, want ...bool) {
	t.Helper()
	if got := sessionLiveFrames(t, app, rec, threadID); !slices.Equal(got, want) {
		t.Fatalf("%s: session_live frames = %v, want %v", stage, got, want)
	}
}

// One frame per change of whether the thread has a session, live or
// starting, through every registry transition; the snapshot set follows.
func TestSessionLivePublishesEachChange(t *testing.T) {
	t.Parallel()
	app, rec := newAppForFlushQueueRPC(t)
	const threadID = "session-live-changes"
	assertSessionLiveFrames(t, app, rec, threadID, "no session")

	start, _ := app.sessionManager().beginStart(threadID)
	assertSessionLiveFrames(t, app, rec, threadID, "starting", true)
	app.sessionManager().put(threadID, session{Provider: string(provider.Claude), Token: "live"})
	app.sessionManager().finishStart(threadID, start)
	assertSessionLiveFrames(t, app, rec, threadID, "registered", true)
	if _, listed := app.sessionLiveThreads()[threadID]; !listed {
		t.Fatal("snapshot set does not name the live thread")
	}

	app.sessionManager().unregister(threadID, "live")
	assertSessionLiveFrames(t, app, rec, threadID, "ended", true, false)
	if _, listed := app.sessionLiveThreads()[threadID]; listed {
		t.Fatal("snapshot set still names the ended thread")
	}

	failed, _ := app.sessionManager().beginStart(threadID)
	assertSessionLiveFrames(t, app, rec, threadID, "starting again", true, false, true)
	app.sessionManager().finishStart(threadID, failed)
	assertSessionLiveFrames(t, app, rec, threadID, "a start that failed", true, false, true, false)
}

// The snapshot names an idle thread that has a session, and a thread with
// other live activity says whether it has one.
func TestListThreadLiveActivityReportsSessionLive(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	idleWithSession := seedLiveStateTodoThread(t, app, "session-idle")
	seedLiveStateTodoThread(t, app, "session-none")
	app.sessionManager().put(idleWithSession.ID, session{Provider: string(provider.Claude), Token: "idle"})
	waitSendsPendingIdle(t, app)

	rows, err := app.ListThreadLiveActivity()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ThreadID != idleWithSession.ID || !rows[0].SessionLive || rows[0].SendsPending || rows[0].ActiveTurn != nil {
		t.Fatalf("rows = %+v, want only %s with a session", rows, idleWithSession.ID)
	}
	if rows[0].ApprovalRequestIDs == nil || rows[0].UserInputRequestIDs == nil {
		t.Fatalf("row = %+v, want empty request lists", rows[0])
	}
}

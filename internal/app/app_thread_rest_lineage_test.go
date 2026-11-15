package app

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A thread is the caller's to stop whole while the caller's spawn links to
// it (store.SpawnLinkOf), however old: a spawn the retention floor reduced
// to lineage, with or without a newer send to the thread, and a spawn older
// than thousands of the caller's later requests.
func TestThreadCancelFindsTheSpawnHoweverOld(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		lineage bool
		send    bool
		newer   int
	}{
		{name: "a spawn reduced to lineage", lineage: true},
		{name: "a spawn reduced to lineage with a newer send", lineage: true, send: true},
		{name: "a spawn behind more than 2000 newer requests", newer: 2001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaudeHoldingTheTurn(t, "working")
			child := f.runningRequest(t, "lead the fix")
			if tc.lineage {
				// The spawn settled, the worker kept working, and the
				// retention floor cleared the spawn's payload.
				execOnBusyStore(t, f.dbPath, fmt.Sprintf(`UPDATE thread_requests
					SET state = 'finished', settled_at = 1, lineage_only = 1, notify = 0, answer = NULL
					WHERE token = '%s'`, child.Token))
			}
			if tc.send {
				if _, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: child.ThreadID, Message: "and this"}); err != nil {
					t.Fatalf("Send: %v", err)
				}
			}
			if tc.newer > 0 {
				other := f.plainThread(t)
				at := time.Now().Add(time.Minute).UnixMilli()
				for i := range tc.newer {
					if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
						Token: fmt.Sprintf("newer-%04d", i), CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
						TargetThreadID: other.ID, State: store.ThreadRequestAccepted, CreatedAt: at + int64(i), UpdatedAt: at + int64(i),
					}); err != nil {
						t.Fatal(err)
					}
				}
			}

			report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{ThreadID: child.ThreadID})
			if err != nil || report.Effect != threadtools.EffectThreadStopped {
				t.Fatalf("Cancel = %+v, %v; want the spawned thread stopped", report, err)
			}
			if _, live := f.app.sessionManager().get(child.ThreadID); live {
				t.Fatal("the spawned thread still has a session")
			}
		})
	}
}

// execOnBusyStore is execOnStore on a connection that waits out the app's
// own writes.
func execOnBusyStore(t *testing.T, path, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

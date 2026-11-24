package app

import (
	"context"
	"path/filepath"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// A review and a manual compaction each start a provider turn. Neither waits
// on its caller: a caller already gone still gets the turn started and
// answered, not one the provider may or may not have been told about.
func TestCodexTurnStartersRunWhenTheirCallerIsGone(t *testing.T) {
	t.Parallel()
	for name, run := range map[string]struct {
		call   func(context.Context, *App, string) error
		method string
	}{
		"review": {
			// A client's send is admitted under its caller's context, so the
			// caller leaves once the send is placed.
			call: func(_ context.Context, app *App, threadID string) error {
				caller, leave := context.WithCancel(context.Background())
				app.sendStageFn = func(_, stage string) {
					if stage == "placed" {
						leave()
					}
				}
				_, err := app.SendMessageWithOptions(caller, threadID, "/review", pageSendOptions(uuid.NewString()))
				return err
			},
			method: `"method":"review/start"`,
		},
		"compact": {
			call: func(ctx context.Context, app *App, threadID string) error {
				return app.CompactCodexThread(ctx, threadID)
			},
			method: `"method":"thread/compact/start"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
			thread := newCodexThreadForReviewTest(t, app, "thread-codex-gone-"+name)
			capturePath := filepath.Join(t.TempDir(), "rpc.ndjson")
			installCodexReviewSession(t, app, thread, "", capturePath)

			gone, cancel := context.WithCancel(context.Background())
			cancel()
			if err := run.call(gone, app, thread.ID); err != nil {
				t.Fatalf("%s with its caller gone = %v, want the turn started", name, err)
			}
			waitForCapturedRequest(t, capturePath, run.method)
		})
	}
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Every start failure names the failed step exactly once, whichever layer
// refused it: the start door adds "start session: " and nothing above or
// below it adds it again.
func TestStartFailuresNameTheStartOnce(t *testing.T) {
	t.Parallel()

	t.Run("a refusal inside the start, reached by a send", func(t *testing.T) {
		t.Parallel()
		app := newTestAppWithStore(t)
		t.Cleanup(func() { _ = app.ServiceShutdown() })
		app.providerBinaryOverride = filepath.Join(t.TempDir(), "no-provider")
		gone := filepath.Join(t.TempDir(), "removed-worktree")
		thread := testThread("thread-start-prefix-send")
		thread.Provider = string(provider.Claude)
		thread.WorkspacePath = gone
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}

		err := app.SendMessage(context.Background(), thread.ID, "hello", nil)
		wantCause := "start session: workspace " + gone + " no longer exists"
		if err == nil || err.Error() != "send message: "+wantCause {
			t.Fatalf("SendMessage() error = %v, want %q", err, "send message: "+wantCause)
		}
		rows := threadErrorRows(t, app.store, thread.ID, "")
		if len(rows) != 1 || rows[0] != "Failed to send: "+wantCause {
			t.Fatalf("error rows = %q, want one reading %q", rows, "Failed to send: "+wantCause)
		}
	})

	t.Run("a transfer refusal at the start door", func(t *testing.T) {
		t.Parallel()
		app := newTestAppWithStore(t)
		thread := testThread("thread-start-prefix-transfer")
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		if _, err := app.store.CreateThreadTransfer(store.ThreadTransfer{
			ID: entityid.New(), ThreadID: thread.ID, PeerBackendID: entityid.New(), Kind: "move",
			Direction: "outgoing", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}

		err := app.StartSession(thread.ID)
		var blocked *store.ThreadTransferError
		if !errors.As(err, &blocked) {
			t.Fatalf("StartSession() error = %v, want the transfer refusal", err)
		}
		if !strings.HasPrefix(err.Error(), "start session: ") || strings.Count(err.Error(), "start session:") != 1 {
			t.Fatalf("StartSession() error = %q, want it to name the start once", err)
		}
	})
}

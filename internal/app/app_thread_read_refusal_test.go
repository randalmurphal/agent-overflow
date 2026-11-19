package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/google/uuid"

	"agent-overflow/internal/store"
)

// Read state follows one rule: MarkThreadRead, MarkThreadsRead and a focus
// stamp refuse the same threads (store.MarkThreadsReadNow's predicate), and
// a provider session's native transfer, which governs execution, is not
// one of them.

func outgoingTransfer(t *testing.T, app *App, threadID string, sessionRefs ...string) {
	t.Helper()
	row, err := app.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: uuid.NewString(), ThreadID: threadID, PeerBackendID: uuid.NewString(), Kind: "move", Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateThreadTransfer: %v", err)
	}
	var refs []store.TransferSession
	for _, ref := range sessionRefs {
		refs = append(refs, store.TransferSession{Provider: "claude", Ref: ref})
	}
	if len(refs) > 0 {
		if err := app.store.BindThreadTransferSessions(row.ID, refs); err != nil {
			t.Fatalf("BindThreadTransferSessions: %v", err)
		}
	}
}

func TestMarkThreadReadIgnoresAnotherThreadsNativeSessionTransfer(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	reader := mustCreateBroadcastThread(t, app)
	mover := mustCreateBroadcastThread(t, app)
	mustCompleteBroadcastTurn(t, app, reader.ID)
	if _, _, err := app.store.MarkThreadUnread(reader.ID); err != nil {
		t.Fatalf("MarkThreadUnread: %v", err)
	}
	// Another thread's move holds a provider session the reader then
	// carries (an alias of it): execution on the reader is refused, which
	// proves the fixture.
	if _, err := app.store.UpdateSessionRef(mover.ID, "shared-session"); err != nil {
		t.Fatalf("UpdateSessionRef(mover): %v", err)
	}
	outgoingTransfer(t, app, mover.ID, "shared-session")
	if _, err := app.store.UpdateSessionRef(reader.ID, "shared-session"); err != nil {
		t.Fatalf("UpdateSessionRef(reader): %v", err)
	}
	if err := app.threadApplication().CheckMutable(reader.ID); err == nil {
		t.Fatal("CheckMutable passed; the native transfer does not hold the reader's session")
	}

	if err := app.MarkThreadRead(reader.ID); err != nil {
		t.Fatalf("MarkThreadRead = %v; read state does not touch the provider session", err)
	}
	if got, err := app.store.GetThread(reader.ID); err != nil || threadUnread(got) {
		t.Fatalf("reader after MarkThreadRead = %+v, %v; want it read", got, err)
	}
	// A thread mid-move is refused, by the batch's rule, as a typed refusal.
	if err := app.MarkThreadRead(mover.ID); !errors.Is(err, store.ErrThreadReadRefused) {
		t.Fatalf("MarkThreadRead(moving) = %v, want store.ErrThreadReadRefused", err)
	}
}

// Focusing a thread this computer may not change is an expected outcome,
// not a failure: the stamp is skipped without a log line. Serial: it
// captures the global logger.
func TestFocusingARefusedThreadLogsNothing(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	app := newTestAppWithStore(t)
	moving := mustCreateBroadcastThread(t, app)
	mustCompleteBroadcastTurn(t, app, moving.ID)
	if _, _, err := app.store.MarkThreadUnread(moving.ID); err != nil {
		t.Fatalf("MarkThreadUnread: %v", err)
	}
	outgoingTransfer(t, app, moving.ID)

	app.markThreadFocused(moving.ID)
	app.markThreadFocused("gone-thread")
	app.waitMarkThreadReads()
	if strings.Contains(logs.String(), "mark thread") {
		t.Fatalf("focus on a refused thread logged:\n%s", logs.String())
	}
	if got, err := app.store.GetThread(moving.ID); err != nil || !threadUnread(got) {
		t.Fatalf("moving thread after focus = %+v, %v; want it untouched", got, err)
	}
}

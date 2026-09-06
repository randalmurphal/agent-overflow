package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
)

func TestTransferFenceRefusesSendAndProviderStartWithoutWritingHistory(t *testing.T) {
	for _, phase := range []string{"preparing", "committed", "complete"} {
		t.Run(phase, func(t *testing.T) {
			app := newTestAppWithStore(t)
			thread := testThread("transfer-thread")
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			operation, err := app.store.CreateThreadTransfer(store.ThreadTransfer{ID: entityid.New(), ThreadID: thread.ID, PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if phase != "preparing" {
				for _, next := range []string{"prepared", "committed", "complete"} {
					if _, err := app.store.AdvanceThreadTransfer(operation.ID, next, strings.Repeat("b", 64)); err != nil {
						t.Fatal(err)
					}
					if next == phase {
						break
					}
				}
			}
			for name, call := range map[string]func() error{"send": func() error { return app.SendMessage(thread.ID, "must remain unsent", nil) }, "start": func() error { return app.StartSession(thread.ID) }} {
				var blocked *store.ThreadTransferError
				if err := call(); !errors.As(err, &blocked) || blocked.OperationID != operation.ID || blocked.Moved != (phase != "preparing") {
					t.Fatalf("%s: %v", name, err)
				}
			}
			items, err := app.store.ListItems(thread.ID)
			if err != nil || len(items) != 0 {
				t.Fatalf("blocked send wrote history: %+v %v", items, err)
			}
			queued, err := app.store.ListFlushQueueItems(thread.ID)
			if err != nil || len(queued) != 0 {
				t.Fatalf("blocked send queued work: %+v %v", queued, err)
			}
		})
	}
}

func TestTransferFenceRejectsNativeAliasBeforeSendSideEffects(t *testing.T) {
	for _, provider := range []string{"claude", "claude-tui", "codex"} {
		t.Run(provider, func(t *testing.T) {
			app := newTestAppWithStore(t)
			alias := testThread("stale-native-alias")
			alias.Provider, alias.SessionRef = provider, "retired-native-session"
			if err := app.store.CreateThread(alias); err != nil {
				t.Fatal(err)
			}
			op, err := app.store.CreateThreadTransfer(store.ThreadTransfer{ID: entityid.New(), ThreadID: alias.ID, PeerBackendID: entityid.New(),
				Kind: "move", Direction: "outgoing", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			nativeProvider := provider
			if nativeProvider == "claude-tui" {
				nativeProvider = "claude"
			}
			if err := app.store.BindThreadTransferSessions(op.ID, []store.TransferSession{{Provider: nativeProvider, Ref: alias.SessionRef}}); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{"prepared", "committed", "complete"} {
				if _, err := app.store.AdvanceThreadTransfer(op.ID, phase, strings.Repeat("b", 64)); err != nil {
					t.Fatal(err)
				}
			}
			// Simulate an old history snapshot restoring a second AO alias.
			alias.ID = "restored-alias"
			if err := app.store.CreateThread(alias); err != nil {
				t.Fatal(err)
			}
			for _, call := range []func() error{func() error { return app.SendMessage(alias.ID, "must not send", nil) }, func() error { return app.StartSession(alias.ID) }} {
				var moved *store.ThreadTransferError
				if err := call(); !errors.As(err, &moved) || !moved.Moved {
					t.Fatalf("native alias executed: %v", err)
				}
			}
			items, err := app.store.ListItems(alias.ID)
			if err != nil || len(items) != 0 {
				t.Fatalf("alias send wrote history: %v %v", items, err)
			}
		})
	}
}

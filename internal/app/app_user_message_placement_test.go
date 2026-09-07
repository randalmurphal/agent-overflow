package app

import (
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

func TestUserMessagePlacement(t *testing.T) {
	for _, p := range []provider.ProviderKind{provider.Claude, provider.Codex} {
		for _, state := range []string{"empty", "rowless-completed", "active", "starting", "completed-late-echo"} {
			t.Run(string(p)+"/"+state, func(t *testing.T) {
				a := newTestAppWithStore(t)
				a.triage = triage.NewRouter(a.store, func(eventchan.Channel, any) {})
				thread := testThread("placement")
				thread.Provider = string(p)
				if err := a.store.CreateThread(thread); err != nil {
					t.Fatal(err)
				}
				next := 0
				if state != "empty" {
					next = 8
					if state != "starting" {
						turn := store.Turn{TurnID: "known", ThreadID: thread.ID, TurnIndex: 7, StartedAt: 1}
						if err := a.store.InsertTurn(turn); err != nil {
							t.Fatal(err)
						}
						if state != "active" {
							if err := a.store.UpdateTurnCompleted("known", 2, "end_turn", "", "", ""); err != nil {
								t.Fatal(err)
							}
						}
					}
					if state == "starting" || state == "completed-late-echo" {
						a.triage.RegisterPendingSendWithExpectation(thread.ID, "user:7", 7, triage.PendingSendExpectation{})
					}
				}
				for _, intent := range []userMessageIntent{messageDirect, messageComposer, messageLegacyComposer, messageSteer, messageFlush, messageFlushFallback} {
					got, err := a.resolveUserMessagePlacement(thread, intent)
					if intent == messageSteer && (p != provider.Codex || state != "active") {
						if err == nil {
							t.Fatalf("intent %d unexpectedly allowed: %+v", intent, got)
						}
						if p == provider.Codex && !errors.Is(err, codex.ErrNoActiveTurn) {
							t.Fatal(err)
						}
						continue
					}
					if intent == messageLegacyComposer && (state == "active" || state == "starting") {
						if err == nil || !strings.Contains(err.Error(), "already working") {
							t.Fatalf("legacy admission: %+v, %v", got, err)
						}
						continue
					}
					if err != nil {
						t.Fatalf("intent %d: %v", intent, err)
					}
					want := userMessagePlacement{displayTurn: next, responseTurn: next}
					switch intent {
					case messageComposer:
						if state == "active" || state == "starting" {
							want = userMessagePlacement{queue: true}
						}
					case messageSteer:
						want = userMessagePlacement{displayTurn: 7, responseTurn: 7}
					case messageFlush, messageFlushFallback:
						want.persistence = messageDeferUntilEcho
						if intent == messageFlush && state == "active" {
							want.displayTurn = 7
							if p == provider.Codex {
								want.responseTurn = 7
							} else {
								want.persistence = messagePersistQuiet
							}
						}
					}
					if got != want {
						t.Errorf("intent %d: got %+v, want %+v", intent, got, want)
					}
				}
			})
		}
	}
}

func TestUserMessageIdentityIndependentOfPlacement(t *testing.T) {
	a := &App{}
	for _, sendID := range []string{"composer-identity", "user:999:flush:../:steer:", "☃"} {
		for _, intent := range []userMessageIntent{messageDirect, messageComposer, messageSteer, messageFlush} {
			first, err := a.userMessageItemID("thread", sendID, 3, intent)
			if err != nil {
				t.Fatal(err)
			}
			next, err := a.userMessageItemID("thread", sendID, 49, intent)
			if err != nil || first != next {
				t.Fatalf("placement changed identity: %q -> %q, %v", first, next, err)
			}
			if _, err := uuid.Parse(first[strings.LastIndex(first, ":")+1:]); err != nil {
				t.Fatalf("not an opaque UUID: %q", first)
			}
			if strings.Contains(first, ":flush:") != (intent == messageFlush) {
				t.Fatalf("client input changed flush grammar: %q", first)
			}
			if intent == messageFlush {
				fallback, _ := a.userMessageItemID("thread", sendID, 4, messageFlushFallback)
				if first != fallback {
					t.Fatalf("fallback changed correlation: %q -> %q", first, fallback)
				}
			}
		}
	}
}

func TestLegacyComposerIdentityRetainsPredictedID(t *testing.T) {
	a := &App{}
	for _, sendID := range []string{"", "already-supported-idempotency-id"} {
		id, err := a.userMessageItemID("thread", sendID, 7, messageLegacyComposer)
		if err != nil || id != "user:7" {
			t.Fatalf("legacy identity = %q, %v", id, err)
		}
	}
}

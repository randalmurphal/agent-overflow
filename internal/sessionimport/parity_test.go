package sessionimport

import (
	"reflect"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// parity_test.go — THE gate.
//
// One synthetic wire sequence is driven twice: through the real
// triage.Router into store A (what a LIVE session persists) and through
// Writer.Build + ApplyImportBatch into store B (what an IMPORT
// persists). Every timeline row, payload, turn, and usage row must
// match. When it stops matching, a row shape changed on exactly one of
// the two writers — which is the bug this test exists to catch, since
// the whole point of `internal/triage`'s exported shape surface is that
// the two share one definition of a row.
//
// There are TWO sequences, because the two providers reach the writer
// through genuinely different vocabularies and a Claude-only fixture
// leaves the Codex-only branches (wire turn ids, settled content blocks,
// the split wait_agent completion, proposed plans) unguarded:
//
//   - `claude` — an implied turn: no wire turn id, streaming text and
//     thinking deltas, an Edit and a Bash, a compaction boundary, an
//     api_error.
//   - `codex` — an explicit turn that NAMES itself (`turn-1`), whole
//     content blocks arriving as EventContentBlockStop rather than
//     deltas, a shell tool, a proposed plan, and a wait_agent whose
//     completion splits into a `tool_completion` sibling.
//
// Normalization is deliberately narrow. Exactly four things are allowed
// to differ, each for a reason that is a property of importing, not a
// shortcut:
//
//  1. `items.meta.import_source_uuid` — provenance a live row cannot
//     have. Dropped before comparison; stampImportProvenance covers it
//     separately.
//  2. Item `created_at` / `updated_at` — a live row's settle stamps
//     time.Now() (doSettleStreamingText / doSettleStreamingThinking),
//     so the live side's clock is the machine's, not the wire's. The
//     import's clock IS the wire's, which is the stronger guarantee and
//     is asserted directly in TestBuildUsesEventTimestampsNotNow. Turn
//     and usage timestamps are NOT normalized — those are compared
//     exactly, because the sidebar's ordering and every usage surface
//     bucket on them.
//  3. Payload ids that are random uuids on BOTH sides (the promoted
//     `tool_call_input` blob, the `compaction` summary blob, the
//     `proposed_plan` blob). Replaced with a `<uuid>` placeholder; the
//     deterministic payload ids are compared verbatim.
//  4. `items.meta` and `payloads.meta` compare as decoded JSON rather
//     than bytes: the import re-marshals the provider's meta object
//     (stripping its own control keys) where the live path stores the
//     wire bytes, so key ORDER can differ where the values cannot.
//
// The fixture user row is seeded with an EMPTY composer meta so the
// live row carries only what triage stamps. A real live send also
// persists a usermessage blob (attachments, draft source) that an
// imported prompt has no equivalent of — that is a difference in what
// AO knows, not in how the row is shaped.

// parityCase is one provider's whole sequence plus the shapes it is
// supposed to exercise. `expectedWireItemID` is what the live send path
// would have registered as the echo it is waiting for: Claude mints the
// uuid the CLI echoes back, Codex echoes no item id at all.
type parityCase struct {
	name               string
	providerName       string
	expectedWireItemID string
	events             func(threadID, workspace string) []provider.ProviderEvent
	wantRows           []parityShape
	assertUsage        func(t *testing.T, usage []store.UsageDetailRow)
}

// parityShape pins one row's identity and payload kinds.
type parityShape struct {
	id, kind, payloadKind, inputPayloadKind string
}

func TestImportWriterMatchesLiveTriageRows(t *testing.T) {
	for _, tc := range []parityCase{claudeParityCase(), codexParityCase()} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			events := tc.events(testThreadID, workspace)

			liveStore := newTestStore(t)
			seedThread(t, liveStore, testThreadID, tc.providerName, workspace)
			driveLiveSession(t, liveStore, events, tc.expectedWireItemID)

			importStore := newTestStore(t)
			thread := seedThread(t, importStore, testThreadID, tc.providerName, workspace)
			if warnings := buildAndApply(t, importStore, thread, importEvents(events)); len(warnings) > 0 {
				t.Fatalf("import produced unexpected warnings: %+v", warnings)
			}

			liveRows := readParityRows(t, liveStore, testThreadID)
			importRows := readParityRows(t, importStore, testThreadID)
			if len(liveRows) != len(importRows) {
				t.Fatalf("row count: live=%d import=%d\nlive=%s\nimport=%s",
					len(liveRows), len(importRows), formatRows(liveRows), formatRows(importRows))
			}
			for i := range liveRows {
				if !reflect.DeepEqual(liveRows[i], importRows[i]) {
					t.Errorf("row %d differs:\n live  = %s\n import= %s",
						i, formatRow(liveRows[i]), formatRow(importRows[i]))
				}
			}

			liveTurns := readParityTurns(t, liveStore, testThreadID)
			importTurns := readParityTurns(t, importStore, testThreadID)
			if !reflect.DeepEqual(liveTurns, importTurns) {
				t.Errorf("turns differ:\n live  = %s\n import= %s", formatAny(liveTurns), formatAny(importTurns))
			}

			liveUsage := readParityUsage(t, liveStore, testThreadID)
			importUsage := readParityUsage(t, importStore, testThreadID)
			if !reflect.DeepEqual(liveUsage, importUsage) {
				t.Errorf("usage ledger differs:\n live  = %s\n import= %s", formatAny(liveUsage), formatAny(importUsage))
			}

			assertParityCoverage(t, tc, importRows)
			tc.assertUsage(t, liveUsage)
		})
	}
}

// assertParityCoverage keeps the comparison honest. Every assertion
// above is an equality between two derived views, and two views that
// both collapse to nothing are trivially equal — a regression that made
// the Edit extractor stop matching, or the compaction payload stop
// being written, would silently turn this test into a no-op. So pin the
// shapes the fixture is supposed to exercise.
func assertParityCoverage(t *testing.T, tc parityCase, rows []parityRow) {
	t.Helper()
	if len(rows) != len(tc.wantRows) {
		t.Fatalf("fixture produced %d rows, want %d: %s", len(rows), len(tc.wantRows), formatRows(rows))
	}
	for i, expect := range tc.wantRows {
		got := rows[i]
		payloadKind, inputPayloadKind := "", ""
		if got.Payload != nil {
			payloadKind = got.Payload.Kind
		}
		if got.InputPayload != nil {
			inputPayloadKind = got.InputPayload.Kind
		}
		if got.ID != expect.id || got.Kind != expect.kind ||
			payloadKind != expect.payloadKind || inputPayloadKind != expect.inputPayloadKind {
			t.Errorf("row %d: got id=%s kind=%s payload=%s inputPayload=%s, want id=%s kind=%s payload=%s inputPayload=%s",
				i, got.ID, got.Kind, payloadKind, inputPayloadKind,
				expect.id, expect.kind, expect.payloadKind, expect.inputPayloadKind)
		}
	}
}

func claudeParityCase() parityCase {
	return parityCase{
		name:               "claude",
		providerName:       "claude",
		expectedWireItemID: "user-uuid-1",
		events:             claudeParityEvents,
		wantRows: []parityShape{
			{"user:1", "user_text", "", ""},
			{"think:1:0", "thinking", "thinking", ""},
			{"text:1:0", "assistant_text", "assistant_text", ""},
			{"toolu_edit_1", "tool_call", "tool_result", "tool_call_input"},
			{"toolu_bash_1", "tool_call", "command_output", ""},
			{"compact:1:provider:compaction-1", "compaction", "compaction", ""},
			{"error:1:0", "api_error", "", ""},
		},
		assertUsage: func(t *testing.T, usage []store.UsageDetailRow) {
			t.Helper()
			if len(usage) != 1 || usage[0].Model != "claude-sonnet-4-5" || usage[0].CostSource != "none" {
				t.Errorf("fixture usage rows are not the shape the ledger comparison assumes: %s", formatAny(usage))
			}
		},
	}
}

func codexParityCase() parityCase {
	return parityCase{
		name:         "codex",
		providerName: "codex",
		// Codex's rollout user message carries no provider item id, so
		// the live send matches its echo FIFO-style rather than by
		// identity — the one branch of consumeMatchingPendingSend the
		// Claude case never reaches.
		expectedWireItemID: "",
		events:             codexParityEvents,
		wantRows: []parityShape{
			{"user:1", "user_text", "", ""},
			{"call_shell_1", "tool_call", "tool_call_result", ""},
			{"text:1:0", "assistant_text", "assistant_text", ""},
			{"think:1:0", "thinking", "thinking", ""},
			{"plan-1", "tool_call", "proposed_plan", ""},
			{"call_wait_1", "tool_call", "", ""},
			{"complete:call_wait_1", "tool_completion", "tool_call_result", ""},
		},
		assertUsage: func(t *testing.T, usage []store.UsageDetailRow) {
			t.Helper()
			if len(usage) != 1 || usage[0].Model != "gpt-5.6-sol" || usage[0].CostSource != "none" {
				t.Errorf("fixture usage rows are not the shape the ledger comparison assumes: %s", formatAny(usage))
			}
		},
	}
}

// driveLiveSession replays the fixture the way a real session produces
// it: the user row is persisted by the send path and registered as a
// pending send BEFORE the wire echo arrives, so triage stamps the
// existing row instead of persisting the echo as an "Injected provider
// context" notification (handle_user_text.go branch 4).
//
// expectedWireItemID is the send path's own answer to "which echo is
// mine": Claude-family sends mint the uuid the CLI echoes back and match
// by identity, while Codex echoes no item id and matches FIFO. Passing
// the wrong one here would not fail loudly — the echo would fall through
// to branch 4 and the live side would grow a notification row — so it is
// a per-provider input, not a constant.
func driveLiveSession(t *testing.T, st *store.Store, events []provider.ProviderEvent, expectedWireItemID string) {
	t.Helper()
	router := triage.NewRouter(st, func(string, any) {})
	t.Cleanup(router.WaitForPendingSettles)

	userEvent := findEvent(t, events, provider.EventUserText)
	userItem := store.Item{
		ID:        "user:1",
		ThreadID:  testThreadID,
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   userEvent.Content,
		CreatedAt: userEvent.Timestamp.UnixMilli(),
		UpdatedAt: userEvent.Timestamp.UnixMilli(),
	}
	if err := router.PersistItem(userItem, nil); err != nil {
		t.Fatalf("persist optimistic user row: %v", err)
	}
	router.RegisterPendingSendExpecting(testThreadID, userItem.ID, userItem.TurnIndex, expectedWireItemID)

	for i, evt := range events {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("live handle event %d (%s): %v", i, evt.Kind, err)
		}
	}
	router.WaitForPendingSettles()
	if err := router.FlushThread(testThreadID); err != nil {
		t.Fatalf("flush live thread: %v", err)
	}
}

func findEvent(t *testing.T, events []provider.ProviderEvent, kind provider.EventKind) provider.ProviderEvent {
	t.Helper()
	for _, evt := range events {
		if evt.Kind == kind {
			return evt
		}
	}
	t.Fatalf("fixture has no %s event", kind)
	return provider.ProviderEvent{}
}

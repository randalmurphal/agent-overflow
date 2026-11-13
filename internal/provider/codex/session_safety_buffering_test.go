package codex

import (
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
)

func safetyBufferingLine(turnID string, show bool) []byte {
	turn := ""
	if turnID != "" {
		turn = fmt.Sprintf(`"turnId":%q,`, turnID)
	}
	return []byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"model/safetyBuffering/updated","params":{"threadId":"codex-thread-1",%s"model":"gpt","reasons":["probes"],"showBufferingUi":%t}}`,
		turn, show,
	))
}

func safetyBufferingNotices(events []provider.ProviderEvent) []provider.ProviderEvent {
	var notices []provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == provider.EventNotification {
			notices = append(notices, evt)
		}
	}
	return notices
}

// Codex derives the signal from every stream frame of a held response, so a
// long answer repeats it once per delta. The transcript gets one row per turn.
func TestSafetyBufferingRaisesOneNoticePerTurn(t *testing.T) {
	t.Parallel()
	s, events := externalTurnTestSession(t, "codex-thread-1")

	for range 500 {
		s.dispatchLine(safetyBufferingLine("turn-1", true))
	}
	notices := safetyBufferingNotices(*events)
	if len(notices) != 1 {
		t.Fatalf("turn-1 raised %d notices, want 1", len(notices))
	}
	if notices[0].TurnID != "turn-1" {
		t.Errorf("notice turn = %q, want turn-1", notices[0].TurnID)
	}

	for range 3 {
		s.dispatchLine(safetyBufferingLine("turn-2", true))
	}
	notices = safetyBufferingNotices(*events)
	if len(notices) != 2 || notices[1].TurnID != "turn-2" {
		t.Fatalf("a held response in the next turn must raise its own notice, got %+v", notices)
	}
}

// showBufferingUi=false produces no row, so it must not spend the turn's notice.
func TestSafetyBufferingHiddenSignalDoesNotSpendTheNotice(t *testing.T) {
	t.Parallel()
	s, events := externalTurnTestSession(t, "codex-thread-1")

	s.dispatchLine(safetyBufferingLine("turn-1", false))
	s.dispatchLine(safetyBufferingLine("turn-1", true))
	s.dispatchLine(safetyBufferingLine("turn-1", false))
	s.dispatchLine(safetyBufferingLine("turn-1", true))

	if notices := safetyBufferingNotices(*events); len(notices) != 1 {
		t.Fatalf("got %d notices, want 1", len(notices))
	}
}

// The wire type requires turnId. A signal without one still gets its row, once.
func TestSafetyBufferingWithoutTurnIDRaisesOneNotice(t *testing.T) {
	t.Parallel()
	s, events := externalTurnTestSession(t, "codex-thread-1")

	for range 3 {
		s.dispatchLine(safetyBufferingLine("", true))
	}
	if notices := safetyBufferingNotices(*events); len(notices) != 1 {
		t.Fatalf("got %d notices, want 1", len(notices))
	}
}

package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func blockedSettingsSession(t *testing.T) (*Session, string, func()) {
	t.Helper()
	capture := filepath.Join(t.TempDir(), "codex-stdin.log")
	gate := filepath.Join(t.TempDir(), "release")
	script := codexTurnCaptureScript(capture)
	marker := `    id=$(`
	if !strings.Contains(script, marker) {
		t.Fatal("capture script changed")
	}
	block := fmt.Sprintf(`    if [[ "$line" == *'"method":"thread/settings/update"'* ]]; then
        while [ ! -f %q ]; do sleep 0.01; done
    fi
`, gate)
	script = strings.Replace(script, marker, block+marker, 1)
	s := newSessionWithScriptCfg(t, script, Config{Model: "gpt-5.5", WorkDir: "/tmp"})
	return s, capture, func() {
		t.Helper()
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func awaitSettingsRequest(t *testing.T, s *Session) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		pending := s.settings.pendingEcho != nil
		s.mu.Unlock()
		if pending {
			return
		}
		select {
		case <-deadline:
			t.Fatal("settings request did not start")
		case <-ticker.C:
		}
	}
}

func TestQueuedSettingsCoalesceAndPreserveLatestTurn(t *testing.T) {
	s, capture, release := blockedSettingsSession(t)
	s.ApplyLiveUpdate(LiveUpdate{Model: "first", ReasoningEffort: "high", ServiceTier: "priority"})
	s.QueueThreadSettings(ThreadSettingsPush{Model: true, Effort: true, ServiceTier: true})
	awaitSettingsRequest(t, s)
	// These calls finish while the provider is deliberately unable to acknowledge.
	s.ApplyLiveUpdate(LiveUpdate{Model: "second", ReasoningEffort: "low"})
	for range 20 {
		s.QueueThreadSettings(ThreadSettingsPush{Model: true, Effort: true, ServiceTier: true})
	}
	release()
	s.settingsSync.wg.Wait()
	pushes := capturedRequestParams(t, capture, threadSettingsUpdateMethod)
	if len(pushes) != 2 {
		t.Fatalf("got %d pushes, want one in flight and one coalesced", len(pushes))
	}
	if pushes[1]["model"] != "second" || pushes[1]["effort"] != "low" {
		t.Fatalf("stale push: %+v", pushes[1])
	}
	if tier, ok := pushes[1]["serviceTier"]; !ok || tier != nil {
		t.Fatalf("OFF did not clear tier: %+v", pushes[1])
	}
	if err := s.Send(context.Background(), "test", provider.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	turns := capturedRequestParams(t, capture, "turn/start")
	if len(turns) != 1 || turns[0]["model"] != "second" || turns[0]["effort"] != "low" {
		t.Fatalf("wrong next turn: %+v", turns)
	}
	s.QueueThreadSettings(ThreadSettingsPush{Model: true})
	s.settingsSync.wg.Wait()
	if len(capturedRequestParams(t, capture, threadSettingsUpdateMethod)) != 2 {
		t.Fatal("pushed into active turn")
	}
}

func TestTimedOutSettingsOnStillClearsOnNextTurn(t *testing.T) {
	s, capture, release := blockedSettingsSession(t)
	SetRequestTimeoutForTest(s, 50*time.Millisecond)
	s.ApplyLiveUpdate(LiveUpdate{Model: "gpt-5.5", ServiceTier: "priority"})
	s.QueueThreadSettings(ThreadSettingsPush{ServiceTier: true})
	s.settingsSync.wg.Wait()
	s.ApplyLiveUpdate(LiveUpdate{Model: "gpt-5.5"})
	release()
	SetRequestTimeoutForTest(s, 3*time.Second)
	if err := s.Send(context.Background(), "test", provider.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	turns := capturedRequestParams(t, capture, "turn/start")
	if len(turns) != 1 {
		t.Fatalf("turns: %+v", turns)
	}
	if tier, ok := turns[0]["serviceTier"]; !ok || tier != nil {
		t.Fatalf("timed-out ON survived OFF: %+v", turns[0])
	}
}

func TestQueuedSettingsRejectionIsTimelineErrorAndCloseJoins(t *testing.T) {
	s, _ := newSessionWithScript(t, codexSettingsUpdateScript(filepath.Join(t.TempDir(), "codex-stdin.log"), "invalid-request"))
	events := make(chan provider.ProviderEvent, 8)
	s.eventMu.Lock()
	s.onEvent = func(evt provider.ProviderEvent) { events <- evt }
	s.eventMu.Unlock()
	s.QueueThreadSettings(ThreadSettingsPush{Model: true})
	s.settingsSync.wg.Wait()
	select {
	case evt := <-events:
		if evt.Kind != provider.EventError {
			t.Fatalf("event: %+v", evt)
		}
	default:
		t.Fatal("rejection was not surfaced")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.QueueThreadSettings(ThreadSettingsPush{Model: true})
	s.settingsSync.mu.Lock()
	defer s.settingsSync.mu.Unlock()
	if s.settingsSync.running || !s.settingsSync.pending.Empty() {
		t.Fatal("closed session retained work")
	}
}

func TestSendDoesNotWaitForSettingsAckAndIgnoresItsLateTierReceipt(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "codex-stdin.log")
	script := strings.Replace(codexTurnCaptureScript(capture), `    id=$(`,
		`    if [[ "$line" == *'"method":"thread/settings/update"'* ]]; then continue; fi
    id=$(`, 1)
	s := newSessionWithScriptCfg(t, script, Config{Model: "gpt-5.5", WorkDir: "/tmp"})
	s.ApplyLiveUpdate(LiveUpdate{Model: "first", ServiceTier: "priority"})
	s.QueueThreadSettings(ThreadSettingsPush{Model: true, ServiceTier: true})
	awaitSettingsRequest(t, s)
	s.ApplyLiveUpdate(LiveUpdate{Model: "latest", ReasoningEffort: "low"})
	done := make(chan error, 1)
	go func() { done <- s.Send(context.Background(), "test", provider.SendOptions{}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send waited for optional settings acknowledgement")
	}
	turns := capturedRequestParams(t, capture, "turn/start")
	if len(turns) != 1 || turns[0]["model"] != "latest" || turns[0]["effort"] != "low" {
		t.Fatalf("wrong turn: %+v", turns)
	}
	if tier, ok := turns[0]["serviceTier"]; !ok || tier != nil {
		t.Fatalf("turn did not clear in-flight ON: %+v", turns[0])
	}
	s.mu.Lock()
	var requestID int64
	for id := range s.pending {
		requestID = id
	}
	s.mu.Unlock()
	if requestID == 0 {
		t.Fatal("expected settings request awaiting acknowledgement")
	}
	s.dispatchLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, requestID)))
	s.settingsSync.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnConfig.assertedServiceTier != "" || s.turnConfig.attemptedServiceTier != "" {
		t.Fatal("late ON acknowledgement undid OFF receipt")
	}
}

func TestCloseCancelsPendingSettingsSynchronization(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "codex-stdin.log")
	script := strings.Replace(codexTurnCaptureScript(capture), `    id=$(`,
		`    if [[ "$line" == *'"method":"thread/settings/update"'* ]]; then continue; fi
    id=$(`, 1)
	s := newSessionWithScriptCfg(t, script, Config{Model: "gpt-5.5", WorkDir: "/tmp"})
	s.QueueThreadSettings(ThreadSettingsPush{Model: true})
	awaitSettingsRequest(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.settingsSync.mu.Lock()
	defer s.settingsSync.mu.Unlock()
	if s.settingsSync.running || !s.settingsSync.pending.Empty() {
		t.Fatal("Close did not join and clear settings work")
	}
}

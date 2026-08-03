package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func fastModeEmissions(emissions *emissionLog) []FastModeStateEvent {
	var out []FastModeStateEvent
	for _, e := range emissions.snapshot() {
		if e.eventName != "provider:fast_mode" {
			continue
		}
		if payload, ok := e.data.(FastModeStateEvent); ok {
			out = append(out, payload)
		}
	}
	return out
}

func initEventWithFastMode(threadID string, status *provider.FastModeStatus) provider.ProviderEvent {
	meta, err := json.Marshal(provider.SessionInfo{SessionID: "s1", Model: "claude-opus-4-7", FastMode: status})
	if err != nil {
		panic(err)
	}
	return provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func TestFastMode_EmittedFromSystemInit(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	err := router.Handle(initEventWithFastMode("t1", &provider.FastModeStatus{
		State:          "off",
		DisabledReason: "sdk_opt_in_required",
	}))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	got := fastModeEmissions(emissions)
	if len(got) != 1 {
		t.Fatalf("fast-mode emissions = %d, want 1", len(got))
	}
	if got[0].ThreadID != "t1" || got[0].State != "off" || got[0].DisabledReason != "sdk_opt_in_required" {
		t.Fatalf("emission = %+v", got[0])
	}
}

// A binary that says nothing about fast mode must produce no frame at
// all: an empty frame would read as "off" in the UI, and absence is
// silence.
func TestFastMode_SilentInitEmitsNothing(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(initEventWithFastMode("t1", nil)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := fastModeEmissions(emissions); len(got) != 0 {
		t.Fatalf("fast-mode emissions = %d, want 0", len(got))
	}
}

func TestFastMode_EmittedFromWireTurnComplete(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnComplete,
		ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason: "end_turn",
			FastMode:   &provider.FastModeStatus{State: "cooldown"},
		},
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	got := fastModeEmissions(emissions)
	if len(got) != 1 {
		t.Fatalf("fast-mode emissions = %d, want 1", len(got))
	}
	if got[0].State != "cooldown" || got[0].DisabledReason != "" {
		t.Fatalf("emission = %+v", got[0])
	}
}

// A soft round close and a synthesized truncation are AO's own signals.
// Neither knows anything about the provider's fast-mode state, so neither
// may produce a frame.
func TestFastMode_NonWireTurnCompleteEmitsNothing(t *testing.T) {
	for name, meta := range map[string]provider.TurnCompleteMeta{
		"soft_round_close": &provider.SoftRoundCloseMeta{StopReason: "end_turn"},
		"truncated":        &provider.TruncatedTurnCompleteMeta{Synthetic: true},
	} {
		t.Run(name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")

			if err := router.Handle(provider.ProviderEvent{
				Kind:         provider.EventTurnComplete,
				ThreadID:     "t1",
				TurnComplete: meta,
				Timestamp:    time.Now(),
			}); err != nil {
				t.Fatalf("turn complete: %v", err)
			}
			if got := fastModeEmissions(emissions); len(got) != 0 {
				t.Fatalf("fast-mode emissions = %d, want 0", len(got))
			}
		})
	}
}

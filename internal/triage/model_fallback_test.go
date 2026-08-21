package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestModelFallbackPersistsWarningAndProjectsEffectiveModel(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.UpdateModel("t1", "claude-fable-5"); err != nil {
		t.Fatalf("seed requested model: %v", err)
	}
	meta, _ := json.Marshal(modelFallbackMeta{
		OriginalModel:          "claude-fable-5",
		FallbackModel:          "claude-opus-4-8",
		Reason:                 "Fable 5's safeguards flagged this message. Switched to Opus 4.8.",
		Trigger:                "refusal",
		Category:               "cyber",
		Explanation:            "security-sensitive request",
		RefusedUserMessageUUID: "user-wire-1",
	})
	evt := provider.ProviderEvent{
		Kind:      provider.EventModelFallback,
		ThreadID:  "t1",
		ItemID:    "model-fallback:req-1",
		Content:   "Fable 5's safeguards flagged this message. Switched to Opus 4.8.",
		Meta:      meta,
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle fallback: %v", err)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.Model != "claude-fable-5" {
		t.Fatalf("durable requested model = %q, want claude-fable-5", thread.Model)
	}
	item, found, err := st.GetThreadItem("t1", evt.ItemID)
	if err != nil || !found {
		t.Fatalf("fallback notification: found=%v err=%v", found, err)
	}
	if item.Kind != itemKindNotification || item.ToolName != modelFallbackNotificationKind {
		t.Fatalf("notification item = %+v", item)
	}
	if item.Summary != evt.Content {
		t.Fatalf("notification summary = %q", item.Summary)
	}
	var itemMeta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &itemMeta); err != nil {
		t.Fatalf("item meta: %v", err)
	}
	if itemMeta["category"] != "cyber" || itemMeta["explanation"] != "security-sensitive request" || itemMeta["fallbackModel"] != "claude-opus-4-8" {
		t.Fatalf("notification meta = %+v", itemMeta)
	}

	snapshot := router.LiveStateSnapshotForThread("t1")
	if snapshot.EffectiveModel != "claude-opus-4-8" {
		t.Fatalf("effective model snapshot = %q", snapshot.EffectiveModel)
	}
	updates := filterEmissions(emissions.snapshot(), "provider:model_fallback")
	if len(updates) != 1 {
		t.Fatalf("model fallback emissions = %+v", emissions.snapshot())
	}
	update := updates[0].data.(ModelFallbackEvent)
	if update.RequestedModel != "claude-fable-5" || update.EffectiveModel != "claude-opus-4-8" || update.Category != "cyber" {
		t.Fatalf("model fallback update = %+v", update)
	}
	if update.Revision == 0 || snapshot.EffectiveModelRevision != update.Revision {
		t.Fatalf("model fallback revision: event=%d snapshot=%d", update.Revision, snapshot.EffectiveModelRevision)
	}
}

func TestModelFallbackClearsWithSessionCleanup(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	meta, _ := json.Marshal(modelFallbackMeta{FallbackModel: "claude-opus-4-8"})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventModelFallback,
		ThreadID:  "t1",
		ItemID:    "model-fallback:req-1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle fallback: %v", err)
	}

	router.CleanupThread("t1")
	if got := router.LiveStateSnapshotForThread("t1").EffectiveModel; got != "" {
		t.Fatalf("effective model after cleanup = %q", got)
	}
	updates := filterEmissions(emissions.snapshot(), "provider:model_fallback")
	if len(updates) != 2 {
		t.Fatalf("model fallback emissions = %+v", emissions.snapshot())
	}
	clear := updates[1].data.(ModelFallbackEvent)
	if clear.ThreadID != "t1" || clear.EffectiveModel != "" {
		t.Fatalf("clear event = %+v", clear)
	}
	set := updates[0].data.(ModelFallbackEvent)
	if clear.Revision <= set.Revision {
		t.Fatalf("clear revision %d must follow set revision %d", clear.Revision, set.Revision)
	}
}

func TestModelFallbackDoesNotProjectLiveModelWhenNotificationPersistenceFails(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	meta, _ := json.Marshal(modelFallbackMeta{FallbackModel: "claude-opus-4-8"})
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventModelFallback,
		ThreadID:  "t1",
		ItemID:    "model-fallback:req-1",
		Meta:      meta,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("handle fallback error = nil after store close")
	}
	if got := router.LiveStateSnapshotForThread("t1").EffectiveModel; got != "" {
		t.Fatalf("effective model after persistence failure = %q", got)
	}
	if updates := filterEmissions(emissions.snapshot(), "provider:model_fallback"); len(updates) != 0 {
		t.Fatalf("unexpected live model projection after persistence failure: %+v", updates)
	}
}

func TestModelFallbackBoundsPersistedProviderFields(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	meta, _ := json.Marshal(modelFallbackMeta{
		OriginalModel:          strings.Repeat("o", maxModelFallbackModelRunes+100),
		FallbackModel:          "claude-opus-4-8",
		Reason:                 strings.Repeat("r", maxModelFallbackReasonRunes+100),
		Trigger:                strings.Repeat("t", maxModelFallbackLabelRunes+100),
		Category:               strings.Repeat("c", maxModelFallbackLabelRunes+100),
		RefusedUserMessageUUID: strings.Repeat("u", maxModelFallbackIDRunes+100),
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventModelFallback,
		ThreadID:  "t1",
		ItemID:    "model-fallback:req-bounds",
		Content:   strings.Repeat("x", maxModelFallbackReasonRunes+100),
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle fallback: %v", err)
	}
	item, found, err := st.GetThreadItem("t1", "model-fallback:req-bounds")
	if err != nil || !found {
		t.Fatalf("fallback notification: found=%v err=%v", found, err)
	}
	if got := len([]rune(item.Summary)); got > maxModelFallbackReasonRunes+3 {
		t.Fatalf("summary runes = %d", got)
	}
	var persisted map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &persisted); err != nil {
		t.Fatalf("item meta: %v", err)
	}
	for field, maxRunes := range map[string]int{
		"originalModel":          maxModelFallbackModelRunes + 3,
		"category":               maxModelFallbackLabelRunes + 3,
		"trigger":                maxModelFallbackLabelRunes + 3,
		"refusedUserMessageUuid": maxModelFallbackIDRunes + 3,
	} {
		value, _ := persisted[field].(string)
		if got := len([]rune(value)); got > maxRunes {
			t.Errorf("%s runes = %d, max %d", field, got, maxRunes)
		}
	}
}

// The three fallback subtypes share one event and one row shape, but not one
// kind: what the row REPORTS is the cause, and flattening a credits-consent or
// availability switch onto the refusal kind told the timeline every fallback
// was a safety refusal.
func TestModelFallbackPersistsItsOwnSubtypeKind(t *testing.T) {
	for _, tc := range []struct {
		wireKind string
		want     string
	}{
		{modelRefusalFallbackNotificationKind, modelRefusalFallbackNotificationKind},
		{modelAvailabilityFallbackKind, modelAvailabilityFallbackKind},
		{modelConsentFallbackKind, modelConsentFallbackKind},
		// A newer CLI's unknown subtype has no frontend branch: it falls back
		// to the refusal kind rather than persisting an unrenderable one.
		{"model_something_new", modelFallbackNotificationKind},
		{"", modelFallbackNotificationKind},
	} {
		t.Run(tc.wireKind, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			meta, _ := json.Marshal(modelFallbackMeta{
				Kind:          tc.wireKind,
				OriginalModel: "claude-fable-5",
				FallbackModel: "claude-opus-4-8",
			})
			evt := provider.ProviderEvent{
				Kind:      provider.EventModelFallback,
				ThreadID:  "t1",
				ItemID:    "model-fallback:kind-1",
				Content:   "Switched to Opus 4.8.",
				Meta:      meta,
				Timestamp: time.Now(),
			}
			if err := router.Handle(evt); err != nil {
				t.Fatalf("handle fallback: %v", err)
			}
			item, found, err := st.GetThreadItem("t1", evt.ItemID)
			if err != nil || !found {
				t.Fatalf("fallback notification: found=%v err=%v", found, err)
			}
			if item.ToolName != tc.want {
				t.Fatalf("persisted kind = %q, want %q", item.ToolName, tc.want)
			}
			var itemMeta map[string]any
			if err := json.Unmarshal([]byte(item.Meta), &itemMeta); err != nil {
				t.Fatalf("item meta: %v", err)
			}
			if itemMeta["kind"] != tc.want {
				t.Fatalf("meta kind = %v, want %q", itemMeta["kind"], tc.want)
			}
		})
	}
}

// model_consent_fallback's own two fields decide whether the switch was
// permanent. Dropped, the row cannot tell "for this session" from "this is
// your default now".
func TestModelConsentFallbackForwardsItsConsentFields(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	meta, _ := json.Marshal(modelFallbackMeta{
		Kind:               modelConsentFallbackKind,
		OriginalModel:      "claude-fable-5",
		FallbackModel:      "claude-opus-4-8",
		Choice:             "fallback",
		PersistedAsDefault: true,
	})
	evt := provider.ProviderEvent{
		Kind:      provider.EventModelFallback,
		ThreadID:  "t1",
		ItemID:    "model-fallback:consent-1",
		Content:   "Switched to Opus 4.8 — now your default model",
		Meta:      meta,
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle consent fallback: %v", err)
	}
	item, found, err := st.GetThreadItem("t1", evt.ItemID)
	if err != nil || !found {
		t.Fatalf("consent notification: found=%v err=%v", found, err)
	}
	var itemMeta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &itemMeta); err != nil {
		t.Fatalf("item meta: %v", err)
	}
	if itemMeta["choice"] != "fallback" {
		t.Fatalf("choice = %v, want fallback (meta=%s)", itemMeta["choice"], item.Meta)
	}
	if persisted, _ := itemMeta["persistedAsDefault"].(bool); !persisted {
		t.Fatalf("persistedAsDefault = %v, want true (meta=%s)", itemMeta["persistedAsDefault"], item.Meta)
	}

	// `false` is not recorded: "for this session only" is what the composed
	// sentence already says, and a false key would be a claim the parser
	// deliberately does not make.
	router2, st2, _ := newTestRouter(t)
	createTestThread(t, st2, "t2")
	sessionOnly, _ := json.Marshal(modelFallbackMeta{
		Kind:          modelConsentFallbackKind,
		FallbackModel: "claude-opus-4-8",
		Choice:        "fallback",
	})
	evt2 := evt
	evt2.ThreadID, evt2.Meta = "t2", sessionOnly
	if err := router2.Handle(evt2); err != nil {
		t.Fatalf("handle session-only consent fallback: %v", err)
	}
	item2, _, err := st2.GetThreadItem("t2", evt2.ItemID)
	if err != nil {
		t.Fatalf("session-only notification: %v", err)
	}
	if strings.Contains(item2.Meta, "persistedAsDefault") {
		t.Fatalf("session-only consent recorded persistedAsDefault: %s", item2.Meta)
	}
}

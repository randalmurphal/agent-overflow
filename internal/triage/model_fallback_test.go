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

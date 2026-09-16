package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// createGeneratedImageThread makes a thread of the named provider; the image
// row is written only for Codex, so the provider column is a gate under test.
func createGeneratedImageThread(t *testing.T, st *store.Store, id, providerName string) {
	t.Helper()
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	err := st.CreateThread(store.Thread{
		ID:            id,
		ProjectID:     triageTestProjectID,
		Title:         "Test",
		Provider:      providerName,
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
}

// imageGenerationMeta is the flat completion meta the Codex adapter builds
// (protocol_meta.go imageGenerationMetaExtras). The base64 `result` field is
// deliberately absent: it never leaves the provider package.
func imageGenerationMeta(status, path, prompt string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"item_status":%q,"toolName":"ImageGeneration","input":{"path":%q,"prompt":%q}}`,
		status, path, prompt))
}

func imageGenerationComplete(threadID, itemID string, meta json.RawMessage) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    itemID,
		ItemType:  "imageGeneration",
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

// recordingImporter counts imports so replay is checked by call count rather
// than by the row alone.
type recordingImporter struct {
	calls  int
	paths  []string
	result GeneratedImage
	err    error
}

func (r *recordingImporter) importer(threadID, savedPath string) (GeneratedImage, error) {
	r.calls++
	r.paths = append(r.paths, savedPath)
	if r.err != nil {
		return GeneratedImage{}, r.err
	}
	return r.result, nil
}

func generatedImageProvenance(t *testing.T, item store.Item) map[string]any {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("decode generated image meta %q: %v", item.Meta, err)
	}
	provenance, ok := meta["generatedImage"].(map[string]any)
	if !ok {
		t.Fatalf("meta has no generatedImage object: %q", item.Meta)
	}
	return provenance
}

func generatedImageAttachments(t *testing.T, item store.Item) []any {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("decode generated image meta %q: %v", item.Meta, err)
	}
	attachments, _ := meta["attachments"].([]any)
	return attachments
}

func TestGeneratedImageRowLandsAtTopLevelAfterToolRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createGeneratedImageThread(t, st, "t1", "codex")
	imports := &recordingImporter{result: GeneratedImage{
		AttachmentID: "att-1", Filename: "generated.png", MimeType: "image/png", Size: 4096,
	}}
	router.SetGeneratedImageImporter(imports.importer)

	insertToolCallItem(t, st, "t1", "img-1", "Generate image", "ImageGeneration", statusRunning)
	evt := imageGenerationComplete("t1", "img-1",
		imageGenerationMeta("completed", "/home/u/.codex/generated_images/a.png", "A quiet dashboard"))
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	if imports.calls != 1 {
		t.Fatalf("importer calls = %d, want 1", imports.calls)
	}
	if imports.paths[0] != "/home/u/.codex/generated_images/a.png" {
		t.Fatalf("imported path = %q", imports.paths[0])
	}

	row, found, err := st.GetThreadItem("t1", GeneratedImageItemID("img-1"))
	if err != nil || !found {
		t.Fatalf("get generated image row: found=%v err=%v", found, err)
	}
	// assistant_text is the kind assistant prose uses, which is what keeps the
	// picture off the activity rail (RAIL_LEAF_KINDS has no case for it).
	if row.Kind != itemKindAssistantText || row.Role != "assistant" {
		t.Fatalf("row kind/role = %q/%q, want assistant_text/assistant", row.Kind, row.Role)
	}
	if row.Status != statusCompleted {
		t.Fatalf("row status = %q, want completed", row.Status)
	}
	if row.Summary != "A quiet dashboard" {
		t.Fatalf("row summary = %q, want the revised prompt", row.Summary)
	}

	tool, _, err := st.GetThreadItem("t1", "img-1")
	if err != nil {
		t.Fatalf("get tool row: %v", err)
	}
	if row.TurnIndex != tool.TurnIndex {
		t.Fatalf("row turn %d != tool turn %d", row.TurnIndex, tool.TurnIndex)
	}
	if row.ItemIndex <= tool.ItemIndex {
		t.Fatalf("row item_index %d must follow tool item_index %d", row.ItemIndex, tool.ItemIndex)
	}

	attachments := generatedImageAttachments(t, row)
	if len(attachments) != 1 {
		t.Fatalf("attachments = %v, want one entry", attachments)
	}
	entry, _ := attachments[0].(map[string]any)
	if entry["id"] != "att-1" || entry["threadId"] != "t1" || entry["kind"] != "image" {
		t.Fatalf("attachment entry = %v", entry)
	}
	if entry["mimeType"] != "image/png" || entry["filename"] != "generated.png" {
		t.Fatalf("attachment entry = %v", entry)
	}
	provenance := generatedImageProvenance(t, row)
	if provenance["sourceItemId"] != "img-1" || provenance["provider"] != "codex" {
		t.Fatalf("provenance = %v", provenance)
	}
	if _, hasError := provenance["error"]; hasError {
		t.Fatalf("successful import must not carry an error: %v", provenance)
	}
}

func TestGeneratedImageRowUsesFallbackSummaryWithoutPrompt(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createGeneratedImageThread(t, st, "t1", "codex")
	router.SetGeneratedImageImporter((&recordingImporter{result: GeneratedImage{
		AttachmentID: "att-1", Filename: "a.png", MimeType: "image/png", Size: 10,
	}}).importer)

	insertToolCallItem(t, st, "t1", "img-1", "Generate image", "ImageGeneration", statusRunning)
	if err := router.Handle(imageGenerationComplete("t1", "img-1",
		imageGenerationMeta("completed", "/gen/a.png", ""))); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	row, found, err := st.GetThreadItem("t1", GeneratedImageItemID("img-1"))
	if err != nil || !found {
		t.Fatalf("get row: found=%v err=%v", found, err)
	}
	if row.Summary != generatedImageSummaryFallback {
		t.Fatalf("summary = %q, want %q", row.Summary, generatedImageSummaryFallback)
	}
}

func TestGeneratedImageReplayImportsOnce(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createGeneratedImageThread(t, st, "t1", "codex")
	imports := &recordingImporter{result: GeneratedImage{
		AttachmentID: "att-1", Filename: "a.png", MimeType: "image/png", Size: 10,
	}}
	router.SetGeneratedImageImporter(imports.importer)

	insertToolCallItem(t, st, "t1", "img-1", "Generate image", "ImageGeneration", statusRunning)
	evt := imageGenerationComplete("t1", "img-1", imageGenerationMeta("completed", "/gen/a.png", "p"))
	for i := 0; i < 3; i++ {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle complete %d: %v", i, err)
		}
	}

	if imports.calls != 1 {
		t.Fatalf("importer calls = %d, want 1 (dedupe on thread+item id)", imports.calls)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	rows := 0
	for _, item := range items {
		if item.ID == GeneratedImageItemID("img-1") {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("generated image rows = %d, want 1", rows)
	}
}

func TestGeneratedImageImportFailureIsVisibleAndRetried(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createGeneratedImageThread(t, st, "t1", "codex")
	imports := &recordingImporter{err: errors.New("source path is outside the permitted directory")}
	router.SetGeneratedImageImporter(imports.importer)

	insertToolCallItem(t, st, "t1", "img-1", "Generate image", "ImageGeneration", statusRunning)
	evt := imageGenerationComplete("t1", "img-1", imageGenerationMeta("completed", "/elsewhere/a.png", "p"))
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	row, found, err := st.GetThreadItem("t1", GeneratedImageItemID("img-1"))
	if err != nil || !found {
		t.Fatalf("a failed import still writes a row: found=%v err=%v", found, err)
	}
	if row.Status != statusErrored {
		t.Fatalf("row status = %q, want errored", row.Status)
	}
	if len(generatedImageAttachments(t, row)) != 0 {
		t.Fatalf("failed import must carry no attachment: %q", row.Meta)
	}
	provenance := generatedImageProvenance(t, row)
	reason, _ := provenance["error"].(string)
	if reason != "source path is outside the permitted directory" {
		t.Fatalf("error reason = %q", reason)
	}

	// An errored row is retried: the failure may have been transient.
	imports.err = nil
	imports.result = GeneratedImage{AttachmentID: "att-1", Filename: "a.png", MimeType: "image/png", Size: 10}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle retry: %v", err)
	}
	if imports.calls != 2 {
		t.Fatalf("importer calls = %d, want 2 (errored row retried)", imports.calls)
	}
	retried, _, err := st.GetThreadItem("t1", GeneratedImageItemID("img-1"))
	if err != nil {
		t.Fatalf("get retried row: %v", err)
	}
	if retried.Status != statusCompleted || len(generatedImageAttachments(t, retried)) != 1 {
		t.Fatalf("retry did not repair the row: status=%q meta=%q", retried.Status, retried.Meta)
	}
}

func TestGeneratedImageRowSkipped(t *testing.T) {
	cases := []struct {
		name         string
		providerName string
		install      bool
		evt          func(threadID string) provider.ProviderEvent
	}{
		{
			name:         "non codex thread",
			providerName: "claude",
			install:      true,
			evt: func(threadID string) provider.ProviderEvent {
				return imageGenerationComplete(threadID, "img-1", imageGenerationMeta("completed", "/gen/a.png", "p"))
			},
		},
		{
			name:         "no importer installed",
			providerName: "codex",
			install:      false,
			evt: func(threadID string) provider.ProviderEvent {
				return imageGenerationComplete(threadID, "img-1", imageGenerationMeta("completed", "/gen/a.png", "p"))
			},
		},
		{
			name:         "another tool",
			providerName: "codex",
			install:      true,
			evt: func(threadID string) provider.ProviderEvent {
				return provider.ProviderEvent{
					Kind:      provider.EventToolComplete,
					ThreadID:  threadID,
					ItemID:    "img-1",
					ItemType:  "webSearch",
					Meta:      json.RawMessage(`{"item_status":"completed","toolName":"WebSearch","input":{"path":"/gen/a.png"}}`),
					Timestamp: time.Now(),
				}
			},
		},
		{
			name:         "no saved path",
			providerName: "codex",
			install:      true,
			evt: func(threadID string) provider.ProviderEvent {
				return imageGenerationComplete(threadID, "img-1", imageGenerationMeta("completed", "", "p"))
			},
		},
		{
			name:         "generation failed",
			providerName: "codex",
			install:      true,
			evt: func(threadID string) provider.ProviderEvent {
				return imageGenerationComplete(threadID, "img-1", imageGenerationMeta("failed", "/gen/a.png", "p"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createGeneratedImageThread(t, st, "t1", tc.providerName)
			imports := &recordingImporter{result: GeneratedImage{
				AttachmentID: "att-1", Filename: "a.png", MimeType: "image/png", Size: 10,
			}}
			if tc.install {
				router.SetGeneratedImageImporter(imports.importer)
			}
			insertToolCallItem(t, st, "t1", "img-1", "Tool", "ImageGeneration", statusRunning)

			if err := router.Handle(tc.evt("t1")); err != nil {
				t.Fatalf("handle: %v", err)
			}
			if imports.calls != 0 {
				t.Fatalf("importer ran %d times, want 0", imports.calls)
			}
			if _, found, err := st.GetThreadItem("t1", GeneratedImageItemID("img-1")); err != nil || found {
				t.Fatalf("generated image row must not exist: found=%v err=%v", found, err)
			}
		})
	}
}

func TestGeneratedImageWireFieldsIgnoreInlineResult(t *testing.T) {
	// The base64 payload is never read, even if a future adapter leaked it
	// into the completion meta.
	raw := json.RawMessage(`{"item_status":"completed","toolName":"ImageGeneration",
		"result":"AAAABBBBCCCC","input":{"path":"/gen/a.png","prompt":"p","result":"DDDD"}}`)
	fields := decodeGeneratedImageWireFields(raw)
	if fields.Path != "/gen/a.png" || fields.Prompt != "p" || fields.Status != "completed" {
		t.Fatalf("decoded = %+v", fields)
	}
	meta := generatedImageMeta("t1", "img-1", fields.Prompt,
		&GeneratedImage{AttachmentID: "att-1", Filename: "a.png", MimeType: "image/png", Size: 10}, "")
	for _, forbidden := range []string{"AAAABBBBCCCC", "DDDD", "result"} {
		if strings.Contains(meta, forbidden) {
			t.Fatalf("stored meta leaked %q: %s", forbidden, meta)
		}
	}
}

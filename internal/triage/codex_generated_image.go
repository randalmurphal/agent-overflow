package triage

// Codex's image-generation tool writes its picture to disk and reports the
// path on the completed item. The tool row stays where every tool row is (in
// the activity run, carrying the lifecycle chip); the PICTURE is assistant
// output and belongs beside assistant prose, so it gets a row of its own at
// the top level, written immediately after the tool row settles.
//
// The row is an `assistant_text` carrying `meta.attachments` — the exact
// shape a user message carries for the images it was sent with. That is not
// an economy: the attachment lives in AO's own store from this point on, so
// serving (thumbnail, lightbox, phone), transfer rewriting
// (itemmeta.TransferAttachments) and export all work on it unchanged, and the
// renderer reuses the tiles and the lightbox the composer path already has.
//
// The base64 `result` field on the wire item is never read here. It never
// reaches triage at all — internal/provider/codex/protocol_meta.go keeps it
// out of Meta on purpose — and nothing in this file asks for it.

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// GeneratedImage is an imported picture reduced to what the timeline row
// needs. It mirrors the `meta.attachments` entry a user message carries.
type GeneratedImage struct {
	AttachmentID string
	Filename     string
	MimeType     string
	Size         int64
}

// GeneratedImageImporter validates a provider-written path and copies its
// bytes into the thread's attachments. The app owns it because resolving the
// provider home is an app-layer responsibility (app_provider_home.go) and
// triage must not learn where a provider keeps its files.
//
// An error is a USER-VISIBLE outcome, not a dropped event: the caller writes
// the row anyway, in `errored` status, carrying the reason.
type GeneratedImageImporter func(threadID, savedPath string) (GeneratedImage, error)

// SetGeneratedImageImporter installs the app-owned importer. Nil (a test
// router, an app that has not finished booting) means generated-image rows
// are not written at all; the tool row still renders its own lifecycle.
func (r *Router) SetGeneratedImageImporter(importer GeneratedImageImporter) {
	r.generatedImageMu.Lock()
	r.generatedImageImporter = importer
	r.generatedImageMu.Unlock()
}

func (r *Router) generatedImageImport() GeneratedImageImporter {
	r.generatedImageMu.Lock()
	defer r.generatedImageMu.Unlock()
	return r.generatedImageImporter
}

// GeneratedImageItemID is the row's deterministic id, derived from the tool
// row it follows. Deterministic because that is what makes the import
// idempotent: a replayed `item/completed` upserts the same row and the
// existence check below skips a second copy of the bytes.
func GeneratedImageItemID(toolItemID string) string { return "image:" + toolItemID }

// codexImageGenerationToolName is what the Codex adapter labels the item with
// (protocol_meta.go imageGenerationMetaExtras).
const codexImageGenerationToolName = "ImageGeneration"

// generatedImageSummaryFallback is the row's text when the provider reported
// no revised prompt. Every summary-reading path (turn preview, title context,
// the nav rail) reads this column, so the row is never blank there.
const generatedImageSummaryFallback = "Generated image"

// isCodexImageGenerationItem reports whether a tool completion is the
// image-generation tool. Both spellings appear on the wire.
func isCodexImageGenerationItem(evt provider.ProviderEvent, toolName string) bool {
	if strings.EqualFold(strings.TrimSpace(toolName), codexImageGenerationToolName) {
		return true
	}
	switch strings.TrimSpace(evt.ItemType) {
	case "imageGeneration", "image_generation":
		return true
	default:
		return false
	}
}

// generatedImageWireFields is the subset of the tool meta this row is built
// from. `input.path` is the saved file; `input.prompt` is the model's revised
// prompt. Nothing else is read.
type generatedImageWireFields struct {
	Path   string
	Prompt string
	Status string
}

func decodeGeneratedImageWireFields(raw json.RawMessage) generatedImageWireFields {
	var decoded struct {
		ItemStatus string `json:"item_status"`
		Input      struct {
			Path   string `json:"path"`
			Prompt string `json:"prompt"`
		} `json:"input"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &decoded) != nil {
		return generatedImageWireFields{}
	}
	return generatedImageWireFields{
		Path:   strings.TrimSpace(decoded.Input.Path),
		Prompt: strings.TrimSpace(decoded.Input.Prompt),
		Status: strings.TrimSpace(decoded.ItemStatus),
	}
}

// persistCodexGeneratedImage writes the top-level picture row for a settled
// image-generation tool item. It runs AFTER persistToolCallCompletion, so the
// tool row already holds its terminal status and this row lands behind it in
// provider order.
//
// Every refusal below is silent on purpose — there is no picture and no
// failure to report: the tool is not image generation, it did not complete,
// it named no file, or the row already exists. An import that FAILS is the
// one case that writes a row, because a user who asked for an image and got
// nothing needs to be told why.
func (r *Router) persistCodexGeneratedImage(evt provider.ProviderEvent) error {
	toolItemID := eventItemID(evt)
	if toolItemID == "" {
		return nil
	}
	meta := DecodeToolCompleteMeta(evt.Meta)
	if !isCodexImageGenerationItem(evt, meta.ToolName) {
		return nil
	}
	codexThread, err := r.isCodexThread(evt.ThreadID)
	if err != nil {
		return err
	}
	if !codexThread {
		return nil
	}
	importer := r.generatedImageImport()
	if importer == nil {
		return nil
	}
	fields := decodeGeneratedImageWireFields(evt.Meta)
	if fields.Path == "" {
		return nil
	}
	// A failed or declined generation has no picture to show; the tool row
	// already carries that outcome.
	if CompletionStatus(meta) != statusCompleted {
		return nil
	}

	itemID := GeneratedImageItemID(toolItemID)
	existing, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("generated image lookup %s: %w", itemID, err)
	}
	// The dedupe key is (thread, tool item id), which `itemID` IS. A replayed
	// completion finds the row it already wrote and imports nothing. A row
	// left in `errored` is retried: the failure may have been transient (the
	// file not yet flushed), and re-importing costs one stat.
	if found && existing.Status == statusCompleted {
		return nil
	}

	launchTurnIndex, err := r.generatedImageTurnIndex(evt, toolItemID)
	if err != nil {
		return err
	}

	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: launchTurnIndex,
		Kind:      itemKindAssistantText,
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   generatedImageSummary(fields.Prompt),
		ParentID:  eventParentID(evt),
		CreatedAt: now,
		UpdatedAt: now,
	}

	imported, importErr := importer(evt.ThreadID, fields.Path)
	if importErr != nil {
		// Visible, with the reason, rather than a broken picture. The log
		// keeps the path (a local filename the user could look at); the row
		// carries the message only.
		log.Printf("triage: import generated image for %s/%s: %v", evt.ThreadID, toolItemID, importErr)
		item.Status = statusErrored
		item.Meta = generatedImageMeta(evt.ThreadID, toolItemID, fields.Prompt, nil, importErr.Error())
	} else {
		item.Meta = generatedImageMeta(evt.ThreadID, toolItemID, fields.Prompt, &imported, "")
	}

	// The picture is a new top-level row arriving while the turn may still be
	// streaming text. Settle the boundary's own scope first so it lands after
	// the prose it follows instead of splitting it (the tool-start rule; a
	// parallel subagent's stream is deliberately left alone).
	r.settleStreamingBeforeTimelineBoundary(evt, "generated image", settleBoundaryScopeOnly)
	return r.persistItem(item, nil)
}

// generatedImageTurnIndex puts the picture in the turn its tool row belongs
// to, so a completion arriving after the next turn opened does not file the
// image under the wrong turn.
func (r *Router) generatedImageTurnIndex(evt provider.ProviderEvent, toolItemID string) (int, error) {
	launch, found, err := r.store.GetThreadItem(evt.ThreadID, toolItemID)
	if err != nil {
		return 0, fmt.Errorf("generated image turn index %s: %w", toolItemID, err)
	}
	if found {
		return launch.TurnIndex, nil
	}
	return r.turnIndexForEvent(evt)
}

func generatedImageSummary(prompt string) string {
	if prompt == "" {
		return generatedImageSummaryFallback
	}
	return prompt
}

// generatedImageMeta is the row's stored metadata. `attachments` is the
// established shape (the same one parseUserMessageAttachments reads and
// itemmeta.TransferAttachments rewrites); `generatedImage` is the provenance
// and the failure reason, which only this row's renderer reads.
func generatedImageMeta(threadID, toolItemID, prompt string, imported *GeneratedImage, failure string) string {
	provenance := map[string]any{"sourceItemId": toolItemID, "provider": "codex"}
	if prompt != "" {
		provenance["prompt"] = prompt
	}
	if failure != "" {
		provenance["error"] = failure
	}
	meta := map[string]any{"generatedImage": provenance}
	if imported != nil {
		meta["attachments"] = []any{map[string]any{
			"id":       imported.AttachmentID,
			"threadId": threadID,
			"filename": imported.Filename,
			"mimeType": imported.MimeType,
			"size":     imported.Size,
			"kind":     "image",
		}}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		// Only unencodable values could fail here and none are reachable;
		// an empty object still yields a visible (if bare) row.
		log.Printf("triage: encode generated image meta for %s: %v", toolItemID, err)
		return "{}"
	}
	return string(encoded)
}

// Package usermessage owns the JSON wire shape persisted in
// store.Item.Meta for user-authored timeline items, plus the
// marshal / unmarshal helpers that every entry point (send, steer,
// flush, fork, revert-to-draft) routes through.
//
// The shape is what the frontend reads back when rendering the user
// row's attachments, source-plan badge, and revision-context badges;
// changes here change what the UI sees, so the JSON tags are part of
// the contract.
package usermessage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// Meta is the JSON shape stored in store.Item.Meta for user_text rows.
// All fields are `omitempty` so a meta row with no attachments and no
// plan / diff revision context serialises to an empty string — the
// caller can then store SQL NULL or skip the Meta write entirely.
type Meta struct {
	Attachments                  []AttachmentMeta             `json:"attachments,omitempty"`
	SourceProposedPlan           *store.ProposedPlanSourceRef `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *store.ProposedPlanSourceRef `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string                     `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *store.DiffReviewSourceRef   `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string                     `json:"revisionSourceDiffCommentIds,omitempty"`
	// Command names the composer slash command that was recognised on
	// this message and expanded into the provider-bound payload (D31),
	// without the leading slash: "workflow" for a message naming
	// `/workflow` at any word position. The stored Summary keeps exactly
	// what the user typed, so this marker is the only record that an
	// expansion happened — chat history colours every occurrence of the
	// word from THIS field rather than from a live registry match, so a
	// row stays truthful about what actually expanded when the registry
	// later changes. One marker per row: naming a command twice expands
	// it once.
	Command string `json:"command,omitempty"`
	// ExpandComposerCommands records that this row came through the public
	// composer path. A flush item can be rebuilt from this durable row after a
	// crash; without the bit that rebuild would treat the user's leading slash
	// as app-injected prose and guard it away from Claude's command router.
	ExpandComposerCommands bool `json:"expandComposerCommands,omitempty"`
}

// Input is the per-entry-point projection Marshal encodes. A struct
// rather than a positional list because every field is optional and
// several are the same type — a caller that swapped two of the
// positional refs used to compile clean.
type Input struct {
	Attachments            []store.Attachment
	SourcePlan             *store.ProposedPlanSourceRef
	RevisionSourcePlan     *store.ProposedPlanSourceRef
	RevisionCommentIDs     []string
	RevisionSourceDiff     *store.DiffReviewSourceRef
	RevisionDiffCommentIDs []string
	Command                string
	ExpandComposerCommands bool
}

// AttachmentMeta is the per-attachment slice element. The Go side
// projects from store.Attachment into this minimal shape so the
// frontend doesn't see internal columns (storage paths, raw hashes,
// timestamps) that aren't relevant for rendering.
type AttachmentMeta struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
	// Kind is store.AttachmentKindImage or store.AttachmentKindFile. It is
	// what the timeline row renders from — an image tile or a file chip —
	// and the MIME type cannot stand in for it: an `image/heic` attachment
	// is a file, because no provider ingests one.
	//
	// omitempty, and EMPTY MEANS IMAGE. Every row written before the kind
	// existed carried an image, so an absent value is the truth about that
	// row rather than a gap to repair.
	Kind string `json:"kind,omitempty"`
}

// Marshal returns the JSON-encoded user-message meta for the given
// attachments + plan/diff revision context + recognised command. When
// every input is zero-valued the function returns ("", nil) so callers
// can persist an empty Meta column and the frontend's omit-empty
// branches continue to work.
func Marshal(in Input) (string, error) {
	if len(in.Attachments) == 0 &&
		in.SourcePlan == nil &&
		in.RevisionSourcePlan == nil &&
		len(in.RevisionCommentIDs) == 0 &&
		in.RevisionSourceDiff == nil &&
		len(in.RevisionDiffCommentIDs) == 0 &&
		in.Command == "" &&
		!in.ExpandComposerCommands {
		return "", nil
	}
	metaAttachments := make([]AttachmentMeta, 0, len(in.Attachments))
	for _, attachment := range in.Attachments {
		metaAttachments = append(metaAttachments, AttachmentMeta{
			ID:       attachment.ID,
			ThreadID: attachment.ThreadID,
			Filename: attachment.Filename,
			MimeType: attachment.MimeType,
			Size:     attachment.Size,
			Kind:     attachment.Kind,
		})
	}
	meta := Meta{
		Attachments:                  metaAttachments,
		SourceProposedPlan:           in.SourcePlan,
		RevisionSourceProposedPlan:   in.RevisionSourcePlan,
		RevisionSourceCommentIDs:     in.RevisionCommentIDs,
		RevisionSourceDiffReview:     in.RevisionSourceDiff,
		RevisionSourceDiffCommentIDs: in.RevisionDiffCommentIDs,
		Command:                      in.Command,
		ExpandComposerCommands:       in.ExpandComposerCommands,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// FromItem decodes the user_text Meta column back into a Meta. An
// empty / whitespace-only Meta returns the zero Meta with no error
// so callers can treat "row was written before meta existed" the
// same as "row deliberately has no meta".
func FromItem(item store.Item) (Meta, error) {
	var meta Meta
	if strings.TrimSpace(item.Meta) == "" {
		return meta, nil
	}
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		return Meta{}, fmt.Errorf("decode user message meta: %w", err)
	}
	return meta, nil
}

// EncodeDraftSource returns the JSON encoding of a source-proposed-plan
// ref suitable for ThreadDraft.PendingPlanImplementation. A nil ref or
// a ref with an empty ItemID returns ("", nil) so the draft stores SQL
// NULL — keeping the partial index idx_thread_drafts_pending_plan_impl
// selective.
func EncodeDraftSource(ref *store.ProposedPlanSourceRef) (string, error) {
	if ref == nil || ref.ItemID == "" {
		return "", nil
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadProviderItemID returns the wire-correlation id stamped onto the
// user_text item's Meta by triage's handle_user_text path
// (`provider_item_id` for Claude is the replay envelope's top-level
// `uuid`; for Codex it is the userMessage item's wire id). Empty when
// the row predates the field, the meta is malformed, or the value
// isn't a string. The field rides as a top-level JSON key on the
// item meta blob alongside the typed Meta fields — it isn't part of
// the Meta struct because it's internal correlation, not UI-facing
// content the frontend should ever read.
//
// The matching writer is MergeProviderItemID below; triage's
// handle_user_text path and the send path both call it directly to set
// the key. All readers and writers agree on the name `provider_item_id`.
func ReadProviderItemID(metaJSON string) string {
	trimmed := strings.TrimSpace(metaJSON)
	if trimmed == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(trimmed), &m) != nil {
		return ""
	}
	id, _ := m["provider_item_id"].(string)
	return id
}

// ReadProviderParentUUID returns the transcript parent uuid stamped
// onto the user_text item's Meta alongside `provider_item_id` (the
// parentUuid the Claude echo reported for the consumed message). Empty
// when the row predates the field, the meta is malformed, or the value
// isn't a string. Like the item id, it rides as a top-level JSON key —
// internal correlation, not UI-facing content.
//
// The durable parent uuid also lives on the message anchor row
// (`provider_parent_uuid`), but that copy is written by a separate
// follow-up UPDATE that can fail after the item-meta stamp committed;
// this copy is stamped in the SAME transaction as `provider_item_id`
// (round-5, R5-8) so the already-cut revert retry — which slices
// through the parent — has a fallback the anchor write can't lose.
func ReadProviderParentUUID(metaJSON string) string {
	trimmed := strings.TrimSpace(metaJSON)
	if trimmed == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(trimmed), &m) != nil {
		return ""
	}
	id, _ := m["provider_parent_uuid"].(string)
	return id
}

// MergeProviderItemID returns a JSON-encoded meta blob that preserves
// every key in `existing` and sets `provider_item_id` to
// providerItemID. Empty providerItemID returns the original meta
// unchanged. Three callers: the fork-time UUID remap (rewrites stored
// wire ids when a Claude session JSONL is forked with fresh uuids), the
// send path (pre-stamps the app-minted id before the provider echo), and
// triage's `handle_user_text` flow (folds the echoed id onto the user
// row). All call it directly — there is no intermediate delegate.
func MergeProviderItemID(existing, providerItemID string) (string, error) {
	return MergeProviderIDs(existing, providerItemID, "")
}

// MergeCommand records a composer command handled as a provider turn while
// preserving attachments and every source-reference field already encoded in
// existing. It is used by built-in commands such as Codex /review whose
// literal text is the durable user row but whose provider input is a dedicated
// RPC rather than that text.
func MergeCommand(existing, command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return existing, nil
	}
	merged := map[string]any{}
	if trimmed := strings.TrimSpace(existing); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &merged); err != nil {
			return "", fmt.Errorf("decode existing meta: %w", err)
		}
		if merged == nil {
			merged = map[string]any{}
		}
	}
	if current, _ := merged["command"].(string); current == command {
		return existing, nil
	}
	merged["command"] = command
	encoded, err := json.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("encode command meta: %w", err)
	}
	return string(encoded), nil
}

// MergeProviderIDs returns a JSON-encoded meta blob that preserves
// every key in `existing` and sets `provider_item_id` /
// `provider_parent_uuid` to the given values in ONE encode — the echo
// stamp writes both in the same store transaction so the parent uuid
// can never be lost to a failed follow-up write (round-5, R5-8). An
// empty value leaves the corresponding key untouched (never blanks a
// stored id); when neither value changes anything the original string
// is returned unchanged so callers can cheaply detect the no-op.
func MergeProviderIDs(existing, providerItemID, parentUUID string) (string, error) {
	if providerItemID == "" && parentUUID == "" {
		return existing, nil
	}
	merged := map[string]any{}
	trimmed := strings.TrimSpace(existing)
	if trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &merged); err != nil {
			return "", fmt.Errorf("decode existing meta: %w", err)
		}
		if merged == nil {
			merged = map[string]any{}
		}
	}
	changed := false
	if providerItemID != "" {
		if cur, ok := merged["provider_item_id"].(string); !ok || cur != providerItemID {
			merged["provider_item_id"] = providerItemID
			changed = true
		}
	}
	if parentUUID != "" {
		if cur, ok := merged["provider_parent_uuid"].(string); !ok || cur != parentUUID {
			merged["provider_parent_uuid"] = parentUUID
			changed = true
		}
	}
	if !changed {
		return existing, nil
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("encode merged meta: %w", err)
	}
	return string(encoded), nil
}

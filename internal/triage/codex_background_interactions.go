package triage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// codex_background_interactions.go — the durable collab-interaction list a
// Codex spawn card renders as sub-lines: the bounded ordered log under
// `codex_collab_interactions`, the digest eviction ledger that keeps the
// upsert idempotent past the cap, the per-child resume-generation counter
// every per-child ordinal is minted from, and the `send_input` completion
// path that appends to it.
//
// The spawn/launch state machine that owns the row this list lives on is in
// codex_background_subagents.go; the mailbox deliveries that append progress
// beats are in codex_background_mailbox.go.
//
// MultiAgentV2 has no per-interaction transcript row. `send_message` and
// `followup_task` both end in one `subAgentActivity kind:"interacted"` item
// (codex-rs/core/src/tools/handlers/multi_agents_v2/message_tool.rs), and the
// child answers on its own schedule. Minting a top-level "Sent input to X" row
// for each one scattered a conversation with one agent across the timeline and
// detached it from the card that owns the agent.
//
// So an interaction is recorded ON the owning spawn launch instead, as a
// bounded, ordered list under `codex_collab_interactions`. The frontend renders
// each entry as a sub-line under the child's card.
//
// `tool` is the RAW function-call name (`send_message` / `followup_task`), which
// the provider carries on `input.activityTool`. It is persisted here, at
// interaction time, because the raw stream is live-only: a resumed session sees
// the typed activity item and nothing else, and the typed wire genuinely cannot
// distinguish the two verbs. Where it is absent the frontend labels the entry
// neutrally — it must NEVER be inferred from whether a child turn followed
// (docs/architecture/invariants.md #25).

// maxCodexCollabInteractions bounds the stored list. It is deliberately larger
// than the frontend's own `maxVisibleCollabInteractions` (8, in
// CollabToolRow.svelte, which renders the last N): the extra headroom is what
// lets the card grow its visible window, or a details view show more, without a
// migration — every entry past this cap is gone from SQLite for good.
const maxCodexCollabInteractions = 32

// The three kind values are WIRE constants: they are persisted in item meta and
// consumed verbatim by the frontend's COLLAB_INTERACTION_KINDS
// (frontend/src/lib/components/chat/collabToolRowData.ts). Renaming a value
// here silently blanks every stored sub-line, so
// TestCodexCollabInteractionKindsMatchTheFrontendMirror parses that file.
const (
	// codexCollabInteractionInteracted is a parent -> child message
	// (`send_message` / `followup_task`).
	codexCollabInteractionInteracted = "interacted"
	// codexCollabInteractionProgress is a child -> parent `MESSAGE` mailbox
	// delivery: the child is reporting progress and is still running.
	codexCollabInteractionProgress = "progress"
	// codexCollabInteractionResumed marks the child starting a new turn after
	// it had already gone terminal.
	codexCollabInteractionResumed = "resumed"
)

type codexCollabInteraction struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Tool string `json:"tool,omitempty"`
	// Text is the bounded body of a child -> parent `MESSAGE` progress note.
	// Empty on every other kind, and on an ENCRYPTED progress envelope, whose
	// payload never leaves the ciphertext.
	Text string `json:"text,omitempty"`
	At   int64  `json:"at"`
}

// codexCollabProgressTextRunes bounds the progress body stored on the card.
// A progress note is a sub-line, not a transcript row: it gets the first line
// and nothing more, because the card has no expansion affordance to reveal the
// rest and item meta is read on every timeline load.
const codexCollabProgressTextRunes = 240

func codexCollabProgressText(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	runes := []rune(line)
	if len(runes) <= codexCollabProgressTextRunes {
		return line
	}
	return strings.TrimSpace(string(runes[:codexCollabProgressTextRunes])) + "\u2026"
}

// decodeCodexChildResumeGenerations reads the per-child resume counter off a
// spawn launch's meta. Absent reads as zero, which is the first turn.
func decodeCodexChildResumeGenerations(raw json.RawMessage) map[string]int {
	var parsed struct {
		Generations map[string]int `json:"codex_child_resume_generations"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &parsed) != nil || parsed.Generations == nil {
		return make(map[string]int)
	}
	return parsed.Generations
}

// codexCollabInteractionLog is the retained list plus the bounded ledger of
// ids that have already fallen off it.
//
// The ledger exists because idempotency and the cap fight each other: the
// upsert is keyed on entry id, but it can only see the RETAINED tail, so a
// duplicate that arrives after its original was trimmed (a reconnect replaying
// an old activity item, a duplicate completion leg) looks new. It would then
// append as the NEWEST sub-line — evicting a real one, and re-dating an
// interaction that happened long ago. Remembering the ids that were dropped is
// what makes the upsert's promise survive the cap.
type codexCollabInteractionLog struct {
	Interactions []codexCollabInteraction
	// Evicted holds one short digest per dropped entry, in eviction order,
	// oldest first, bounded by maxCodexCollabInteractionsEvicted. Rows written
	// before the digest form hold the raw ids instead; both are matched (see
	// codexCollabInteractionWasEvicted).
	Evicted []string
}

// maxCodexCollabInteractionsEvicted bounds the replay ledger, and is
// deliberately NOT tied to maxCodexCollabInteractions.
//
// It used to be `= maxCodexCollabInteractions`, which made the ledger share
// the RENDERED list's retention horizon: 32 retained + 32 remembered meant the
// 65th unique interaction on one card pushed the first one out of both, and a
// replay of it appended as the newest sub-line — evicting a live entry and
// re-dating an interaction that happened long ago, which is the exact failure
// the ledger exists to prevent. Uniqueness state has no reason to age at the
// speed the card's window does.
//
// It cannot be unbounded — the ledger lives in items.meta, which is read on
// every timeline load — so this is a horizon either way. What it is NOT is the
// retention cap: entries are stored as
// codexCollabInteractionEvictedDigestBytes-long digests rather than whole
// provider ids, so four times the horizon costs less than the raw-id ledger it
// replaces, and the meta only grows at all once a card has passed the
// retention cap.
const maxCodexCollabInteractionsEvicted = 128

// codexCollabInteractionEvictedDigestBytes is how much of an evicted id's
// sha256 the ledger keeps. Four bytes over 128 entries is a ~7.6e-6 chance of
// one collision, and a collision costs one dropped sub-line, never a wrong
// one — the same direction the ledger already errs in.
const codexCollabInteractionEvictedDigestBytes = 4

// codexCollabInteractionEvictedDigest is the ledger's stored form of an entry
// id. Ids are opaque provider values (a `send_input` tool call id) and content
// hashes (`progress:<hash>`), never ordinals, so there is no watermark that
// could stand in for remembering them one by one.
func codexCollabInteractionEvictedDigest(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:codexCollabInteractionEvictedDigestBytes])
}

// codexCollabInteractionWasEvicted reports whether this id already fell off
// the retained list. It accepts BOTH forms the column can hold: the digest
// written now, and the raw id written by rows that predate it — an id and its
// own digest can never both be wrong about the same entry, so matching either
// is exact rather than a widening.
func codexCollabInteractionWasEvicted(evicted []string, id string) bool {
	digest := codexCollabInteractionEvictedDigest(id)
	for _, entry := range evicted {
		if entry == digest || entry == id {
			return true
		}
	}
	return false
}

func decodeCodexCollabInteractionLog(raw json.RawMessage) codexCollabInteractionLog {
	var parsed struct {
		Interactions []codexCollabInteraction `json:"codex_collab_interactions"`
		Evicted      []string                 `json:"codex_collab_interactions_evicted"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &parsed) != nil {
		return codexCollabInteractionLog{}
	}
	return codexCollabInteractionLog{Interactions: parsed.Interactions, Evicted: parsed.Evicted}
}

func codexCollabInteractionsMeta(log codexCollabInteractionLog) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"codex_collab_interactions":         log.Interactions,
		"codex_collab_interactions_evicted": log.Evicted,
	})
}

// appendCodexCollabInteraction upserts one interaction onto a spawn launch.
// Idempotent by entry id: the same activity item replayed (reconnect, duplicate
// completion leg) updates in place rather than appending a second sub-line.
func (r *Router) appendCodexCollabInteraction(launch store.Item, entry codexCollabInteraction) error {
	if strings.TrimSpace(entry.ID) == "" {
		return nil
	}
	log, changed := mergeCodexCollabInteraction(
		decodeCodexCollabInteractionLog(json.RawMessage(launch.Meta)),
		entry,
	)
	if !changed {
		return nil
	}
	meta, err := codexCollabInteractionsMeta(log)
	if err != nil {
		return fmt.Errorf("codex-background collab interactions meta %s: %w", launch.ID, err)
	}
	launch.Meta = mergeItemMetaJSON(launch.Meta, meta)
	launch.UpdatedAt = time.Now().UnixMilli()
	return r.persistItem(launch, nil)
}

// mergeCodexCollabInteraction is the upsert-and-bound rule both writers share —
// appendCodexCollabInteraction and reactivateCodexSpawnChild, which builds its
// resume entry into a larger meta merge and so cannot call the persisting
// wrapper. It reports false when the list is already exactly this entry, so the
// caller can skip a pointless write.
func mergeCodexCollabInteraction(
	log codexCollabInteractionLog,
	entry codexCollabInteraction,
) (codexCollabInteractionLog, bool) {
	replaced := false
	for i, existing := range log.Interactions {
		if existing.ID != entry.ID {
			continue
		}
		if existing == entry {
			return log, false
		}
		log.Interactions[i] = entry
		replaced = true
		break
	}
	if !replaced {
		// A duplicate of an entry the cap already dropped is a REPLAY, not a
		// new interaction: re-appending it would evict a live sub-line and
		// re-date an interaction that happened long ago. Its content is gone
		// for good either way, so there is nothing to update in place.
		if codexCollabInteractionWasEvicted(log.Evicted, entry.ID) {
			return log, false
		}
		log.Interactions = append(log.Interactions, entry)
	}
	if len(log.Interactions) > maxCodexCollabInteractions {
		dropped := log.Interactions[:len(log.Interactions)-maxCodexCollabInteractions]
		for _, evicted := range dropped {
			log.Evicted = append(log.Evicted, codexCollabInteractionEvictedDigest(evicted.ID))
		}
		if len(log.Evicted) > maxCodexCollabInteractionsEvicted {
			log.Evicted = log.Evicted[len(log.Evicted)-maxCodexCollabInteractionsEvicted:]
		}
		log.Interactions = log.Interactions[len(log.Interactions)-maxCodexCollabInteractions:]
	}
	return log, true
}

// codexCollabLaunchForInteraction resolves the spawn card an interaction belongs
// to. ParentToolUseID is the V2 answer — the provider joins the activity item's
// agentThreadId to the launch that owns that child. The receiverThreadIds walk
// is the V1 fallback, where `collabAgentToolCall sendInput` names its receivers
// directly and no parent link is on the wire.
func (r *Router) codexCollabLaunchForInteraction(evt provider.ProviderEvent) (persistedCodexSpawnLaunch, bool, error) {
	if parentID := strings.TrimSpace(evt.ParentToolUseID); parentID != "" {
		return r.findPersistedCodexSpawnLaunchForStatus(evt.ThreadID, parentID, "")
	}
	meta := decodeCodexItemMeta(evt.Meta)
	for _, childID := range meta.ReceiverThreadIDs {
		childID = strings.TrimSpace(childID)
		if childID == "" {
			continue
		}
		launch, found, err := r.findPersistedCodexSpawnLaunch(evt.ThreadID, "", childID, true)
		if err != nil || found {
			return launch, found, err
		}
	}
	return persistedCodexSpawnLaunch{}, false, nil
}

// observeCodexCollabInteractionComplete lands a `send_input` completion on its
// spawn card. Returns true when the event was claimed, so the caller knows the
// row was accounted for and does not need a top-level fallback.
func (r *Router) observeCodexCollabInteractionComplete(evt provider.ProviderEvent) (bool, error) {
	if evt.ItemType != "send_input" {
		return false, nil
	}
	launch, found, err := r.codexCollabLaunchForInteraction(evt)
	if err != nil || !found {
		return false, err
	}
	entry := codexCollabInteraction{
		ID:   strings.TrimSpace(eventItemID(evt)),
		Kind: codexCollabInteractionInteracted,
		Tool: codexCollabActivityTool(evt.Meta),
		At:   eventTimestampMillis(evt),
	}
	if err := r.appendCodexCollabInteraction(launch.item, entry); err != nil {
		return false, err
	}
	return true, nil
}

// codexCollabActivityTool reads the raw function-call name off the enriched
// item meta (`input.activityTool`). Empty when the raw stream was unavailable —
// a resumed session — and the caller must then stay neutral rather than guess.
func codexCollabActivityTool(raw json.RawMessage) string {
	var shell struct {
		Input struct {
			ActivityTool string `json:"activityTool"`
		} `json:"input"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &shell) != nil {
		return ""
	}
	switch tool := strings.TrimSpace(shell.Input.ActivityTool); tool {
	case "send_message", "followup_task":
		return tool
	default:
		return ""
	}
}

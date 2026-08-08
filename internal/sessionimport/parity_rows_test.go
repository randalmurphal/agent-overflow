package sessionimport

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"agent-overflow/internal/store"
)

// parity_rows_test.go — the read-back and normalization half of the
// parity gate. What each normalization is allowed to hide, and why, is
// documented in parity_test.go's header; this file only implements it.

// parityRow is one timeline row reduced to the fields both writers own.
// Timestamps are deliberately absent — see the file header.
type parityRow struct {
	ID           string
	TurnIndex    int
	ItemIndex    int
	Kind         string
	Role         string
	Status       string
	Summary      string
	ParentID     string
	CompletionOf string
	ToolName     string
	Decision     string
	IsBackground bool
	Meta         any
	Payload      *parityPayload
	InputPayload *parityPayload
}

type parityPayload struct {
	ID   string
	Kind string
	Meta any
	Data string
}

func readParityRows(t *testing.T, st *store.Store, threadID string) []parityRow {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	rows := make([]parityRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, parityRow{
			ID:           item.ID,
			TurnIndex:    item.TurnIndex,
			ItemIndex:    item.ItemIndex,
			Kind:         item.Kind,
			Role:         item.Role,
			Status:       item.Status,
			Summary:      item.Summary,
			ParentID:     item.ParentID,
			CompletionOf: item.CompletionOf,
			ToolName:     item.ToolName,
			Decision:     item.Decision,
			IsBackground: item.IsBackground,
			Meta:         normalizeMeta(t, stripImportProvenance(t, item.Meta)),
			Payload:      readParityPayload(t, st, item.PayloadID),
			InputPayload: readParityPayload(t, st, item.InputPayloadID),
		})
	}
	return rows
}

func readParityPayload(t *testing.T, st *store.Store, payloadID string) *parityPayload {
	t.Helper()
	if payloadID == "" {
		return nil
	}
	meta, err := st.GetPayloadMeta(payloadID)
	if err != nil {
		t.Fatalf("payload meta %s: %v", payloadID, err)
	}
	data, err := st.GetPayloadData(payloadID)
	if err != nil {
		t.Fatalf("payload data %s: %v", payloadID, err)
	}
	return &parityPayload{
		ID:   maskUUIDs(meta.ID),
		Kind: meta.Kind,
		Meta: normalizeMeta(t, meta.Meta),
		Data: string(data),
	}
}

type parityTurn struct {
	TurnID             string `json:"turnId"`
	TurnIndex          int    `json:"turnIndex"`
	StartedAt          int64  `json:"startedAt"`
	CompletedAt        int64  `json:"completedAt"`
	StopReason         string `json:"stopReason,omitempty"`
	AssistantMessageID string `json:"assistantMessageId,omitempty"`
	TokenUsage         any    `json:"tokenUsage,omitempty"`
	ErrorMessage       string `json:"errorMessage,omitempty"`
	ProviderTurnID     string `json:"providerTurnId,omitempty"`
}

func readParityTurns(t *testing.T, st *store.Store, threadID string) []parityTurn {
	t.Helper()
	turns, err := st.ListRecentTurns(threadID, 50)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	out := make([]parityTurn, 0, len(turns))
	for _, turn := range turns {
		if turn.CompletedAt == nil {
			t.Fatalf("turn %s has a NULL completed_at; the boot sweep would flip it to interrupted", turn.TurnID)
		}
		out = append(out, parityTurn{
			TurnID:             turn.TurnID,
			TurnIndex:          turn.TurnIndex,
			StartedAt:          turn.StartedAt,
			CompletedAt:        *turn.CompletedAt,
			StopReason:         turn.StopReason,
			AssistantMessageID: turn.AssistantMessageID,
			TokenUsage:         normalizeMeta(t, turn.TokenUsageJSON),
			ErrorMessage:       turn.ErrorMessage,
			ProviderTurnID:     turn.ProviderTurnID,
		})
	}
	return out
}

// readParityUsage groups by calendar DAY on purpose: the bucket key is
// derived from usage_ledger.created_at, so a side that restamped the
// import with now() lands in a different bucket and fails here rather
// than blending into an untimed aggregate. The exact millisecond is
// pinned separately by TestBuildUsesEventTimestampsNotNow.
func readParityUsage(t *testing.T, st *store.Store, threadID string) []store.UsageDetailRow {
	t.Helper()
	rows, err := st.QueryUsageDetail(store.UsageQuery{ThreadID: threadID, GroupBy: "day"})
	if err != nil {
		t.Fatalf("query usage detail: %v", err)
	}
	return rows
}

// stripImportProvenance removes the import-only provenance key. It is
// the one items.meta key a live row cannot carry, and
// TestBuildStampsImportProvenance asserts its presence separately.
func stripImportProvenance(t *testing.T, meta string) string {
	t.Helper()
	if meta == "" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(meta), &obj); err != nil {
		return meta
	}
	if _, ok := obj["import_source_uuid"]; !ok {
		return meta
	}
	delete(obj, "import_source_uuid")
	encoded, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("re-encode meta without provenance: %v", err)
	}
	return string(encoded)
}

// normalizeMeta compares meta columns as JSON values rather than bytes.
func normalizeMeta(t *testing.T, meta string) any {
	t.Helper()
	if meta == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(meta), &value); err != nil {
		// Not JSON: compare the raw text, which is still a real
		// difference if the two writers disagree.
		return meta
	}
	return value
}

// bareUUIDPattern is ANCHORED on purpose. The only payload ids that
// legitimately differ between the two writers are the ones both mint
// from `uuid.NewString()` — nothing else, so the whole id is a uuid and
// nothing more. An unanchored match would also blank a uuid EMBEDDED in
// a deterministic id (`compact:1:provider:<uuid>` is the live shape when
// the provider names a compaction by uuid), which is exactly the half of
// the id that proves the two writers agree on how it is derived.
var bareUUIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// maskUUIDs replaces a payload id that is nothing BUT a random uuid (the
// promoted tool_call_input blob, the compaction summary blob, the plan
// blob). Every other id is deterministic on both sides and is compared
// verbatim, uuid-shaped substrings included.
func maskUUIDs(id string) string {
	if bareUUIDPattern.MatchString(id) {
		return "<uuid>"
	}
	return id
}

func formatRow(row parityRow) string     { return formatAny(row) }
func formatRows(rows []parityRow) string { return formatAny(rows) }

func formatAny(value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%+v", value)
	}
	return string(encoded)
}

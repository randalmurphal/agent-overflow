package store

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/transferfiles"
)

// ImportThreadHistory installs a validated local snapshot into a NEW thread in
// one transaction, as a copy. The caller supplies destination workspace/provider settings;
// the archive cannot name another project's execution target. No credentials,
// host settings, usage ledger rows or derived rendering caches are imported.
func (s *Store) ImportThreadHistory(ctx context.Context, target Thread, input io.Reader) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.importThreadHistoryTx(ctx, tx, target, input, false); err != nil {
		return err
	}
	return tx.Commit()
}

// importThreadHistoryTx creates the thread and loads its history under
// history_bulk_load, so the copy counts nothing (thread_tallies.go); a move
// installs the tallies its archive carries (installTransferTalliesTx).
func (s *Store) importThreadHistoryTx(ctx context.Context, tx *sql.Tx, target Thread, input io.Reader, move bool) error {
	prepared, lastReadAt, err := prepareThreadForCreate(target)
	if err != nil {
		return err
	}
	if err := insertThread(tx, prepared, lastReadAt); err != nil {
		return err
	}
	if err := setHistoryBulkLoadTx(tx, target.ID, true, "transfer import"); err != nil {
		return err
	}
	tallies, err := readTransferHistoryTx(ctx, tx, target, input)
	if err != nil {
		return err
	}
	// The rows carry no subagent card; the thread's stamps are built once,
	// from the whole copy.
	if err := s.restampSubagentAggregatesTx(tx, target.ID); err != nil {
		return err
	}
	if err := installTransferTalliesTx(tx, target.ID, move, tallies); err != nil {
		return err
	}
	return setHistoryBulkLoadTx(tx, target.ID, false, "transfer import")
}

// installTransferTalliesTx gives a moved thread the tallies its archive
// carries, in place of what this computer held for it, so a thread that
// moves away and back keeps the work it did on both. A copy is a new
// thread that has done nothing yet, and an archive from a sender that
// carries none installs none.
func installTransferTalliesTx(tx *sql.Tx, threadID string, move bool, tallies *transferTallies) error {
	if !move || tallies == nil {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO thread_tallies (thread_id, tool_calls, tokens, turn_ms) VALUES (?, ?, ?, ?)
		ON CONFLICT (thread_id) DO UPDATE SET tool_calls = excluded.tool_calls, tokens = excluded.tokens, turn_ms = excluded.turn_ms`,
		threadID, max(0, tallies.ToolCalls), max(0, tallies.Tokens), max(0, tallies.TurnMs)); err != nil {
		return fmt.Errorf("transfer: install the tallies of %s: %w", threadID, err)
	}
	return nil
}

// readTransferHistoryTx loads the archive's history into target and returns
// the tallies its header carries, nil when it carries none.
func readTransferHistoryTx(ctx context.Context, tx *sql.Tx, target Thread, input io.Reader) (*transferTallies, error) {
	limited := &io.LimitedReader{R: input, N: transferfiles.MaxFileBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), transferHistoryRecordLimit)
	var sourceID string
	var tallies *transferTallies
	var err error
	attachments := make(map[string]itemmeta.AttachmentDestination)
	var payload transferPayloadMeta
	var offset, index int
	count := 0
	ended := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count++
		if count > transferHistoryRowLimit*4 {
			return nil, errors.New("transfer: history exceeds the record limit")
		}
		if ended {
			return nil, errors.New("transfer: data after history end marker")
		}
		var record transferHistoryRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		if record.Version != transferHistoryVersion {
			return nil, errors.New("transfer: unsupported history format; update this computer first")
		}
		if count == 1 && record.Kind != "thread" {
			return nil, errors.New("transfer: missing conversation header")
		}
		if record.Kind != "payload_chunk" && payload.ID != "" {
			if offset != payload.Size {
				return nil, errors.New("transfer: incomplete payload data")
			}
			payload = transferPayloadMeta{}
		}
		switch record.Kind {
		case "thread":
			if count != 1 {
				return nil, errors.New("transfer: duplicate conversation header")
			}
			var header transferThreadHeader
			if err := json.Unmarshal(record.Data, &header); err != nil {
				return nil, err
			}
			source := header.Thread
			tallies = header.Tallies
			if source.ID == "" || source.Provider != target.Provider {
				return nil, errors.New("transfer: conversation provider does not match the destination")
			}
			sourceID = source.ID
		case "payload":
			if err := json.Unmarshal(record.Data, &payload); err != nil {
				return nil, err
			}
			if payload.ID == "" || payload.Size < 0 || int64(payload.Size) > 2<<30 {
				return nil, errors.New("transfer: payload has no identity")
			}
			offset, index = 0, 0
			if err := insertPayloadTx(tx, target.ID, Payload{ID: payload.ID, Kind: payload.Kind, Meta: payload.Meta, Data: []byte{}, CreatedAt: payload.CreatedAt}, "transfer payload"); err != nil {
				return nil, err
			}
		case "payload_chunk":
			var chunk transferPayloadChunk
			if err := json.Unmarshal(record.Data, &chunk); err != nil {
				return nil, err
			}
			if payload.ID == "" || chunk.ID != payload.ID || chunk.Offset != offset || len(chunk.Data) == 0 || len(chunk.Data) > transferHistoryChunk || len(chunk.Data) > payload.Size-offset {
				return nil, errors.New("transfer: payload chunks are missing, reordered or oversized")
			}
			_, err := tx.Exec(`INSERT INTO payload_chunks (thread_id,payload_id,chunk_index,start_offset,data,created_at) VALUES (?,?,?,?,?,?)`, target.ID, payload.ID, index, offset, chunk.Data, payload.CreatedAt)
			if err != nil {
				return nil, err
			}
			offset += len(chunk.Data)
			index++
		case "item":
			var item Item
			if err := json.Unmarshal(record.Data, &item); err != nil {
				return nil, err
			}
			if item.ThreadID != sourceID || item.ID == "" {
				return nil, errors.New("transfer: item belongs to another conversation")
			}
			item.ThreadID, item.PayloadPreviewSpans = target.ID, ""
			// Attachment references are rewritten on every kind that can
			// hold them. A user message carries the images it was sent
			// with; an assistant row carries a picture the agent generated
			// and AO imported (triage/codex_generated_image.go). Both
			// reference rows whose ids this transfer reallocated, so both
			// must be remapped or the copy points at the source computer's
			// attachment ids. Narrowed to those two kinds rather than run
			// over every row: a tool row's meta is provider wire content,
			// and a top-level `attachments` key appearing there would be
			// something else entirely.
			if item.Kind == "user_text" || item.Kind == "assistant_text" {
				item.Meta, err = itemmeta.TransferAttachments(item.Meta, sourceID, attachments)
				if err != nil {
					return nil, err
				}
			}
			if item.Kind == "user_text" {
				item.Meta, err = itemmeta.TransferThreadReferences(item.Meta, sourceID, target.ID, func(kind, id string) string { return transferContentID(target.ID, kind, id) })
				if err != nil {
					return nil, err
				}
			}
			// The caller rebuilds the thread's subagent stamps from the whole
			// copy (restampSubagentAggregatesTx).
			if _, err := tx.Exec(itemInsertSQL, itemInsertArgs(item)...); err != nil {
				return nil, fmt.Errorf("transfer item %s: %w", item.ID, err)
			}
			if err := indexSettledItemTx(tx, item.ThreadID, item.ID, item.Kind, item.Status, item.Summary); err != nil {
				return nil, err
			}
		case "turn":
			var turn Turn
			if err := json.Unmarshal(record.Data, &turn); err != nil {
				return nil, err
			}
			if turn.ThreadID != sourceID || turn.TurnID == "" || turn.CompletedAt == nil {
				return nil, errors.New("transfer: turn is still running or belongs to another conversation")
			}
			turn.TurnID = transferredTurnID(sourceID, target.ID, turn.TurnID)
			_, err := tx.Exec(`INSERT INTO turns (`+turnColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`, turn.TurnID, target.ID, turn.TurnIndex, turn.StartedAt, turn.CompletedAt, turn.StopReason, turn.AssistantMessageID, turn.TokenUsageJSON, turn.ErrorMessage, turn.ProviderTurnID)
			if err != nil {
				return nil, err
			}
		case "anchor":
			var anchor MessageAnchor
			if err := json.Unmarshal(record.Data, &anchor); err != nil {
				return nil, err
			}
			if anchor.ThreadID != sourceID {
				return nil, errors.New("transfer: message anchor belongs to another conversation")
			}
			_, err := tx.Exec(`INSERT INTO message_anchors (`+messageAnchorColumns+`) VALUES (?,?,?,?,?,?)`, target.ID, anchor.UserItemID, anchor.TurnIndex, anchor.ProviderUserMessageID, anchor.ProviderParentUUID, anchor.CreatedAt)
			if err != nil {
				return nil, err
			}
		case "attachment":
			var a Attachment
			if err := json.Unmarshal(record.Data, &a); err != nil {
				return nil, err
			}
			if a.ID == "" || validateAttachment(a) != nil || len(attachments) >= 16_384 {
				return nil, errors.New("transfer: invalid attachment metadata")
			}
			original := a
			a, err = TransferredAttachment(a, target.ID)
			if err != nil {
				return nil, err
			}
			if _, exists := attachments[original.ID]; exists {
				return nil, errors.New("transfer: duplicate attachment metadata")
			}
			attachments[original.ID] = itemmeta.AttachmentDestination{SourceThreadID: original.ThreadID, ThreadID: target.ID, ID: a.ID}
			if err := importTransferAttachment(tx, a); err != nil {
				return nil, err
			}
		case "draft":
			var draft ThreadDraft
			if err := json.Unmarshal(record.Data, &draft); err != nil {
				return nil, err
			}
			if draft.ThreadID != sourceID || !json.Valid([]byte(draft.Attachments)) {
				return nil, errors.New("transfer: invalid composer draft")
			}
			draft.Attachments, err = itemmeta.TransferAttachmentArray(draft.Attachments, sourceID, attachments)
			if err != nil {
				return nil, err
			}
			if draft.PendingPlanImplementation != "" {
				wrapped, err := itemmeta.TransferThreadReferences(`{"sourceProposedPlan":`+draft.PendingPlanImplementation+`}`, sourceID, target.ID, func(kind, id string) string { return transferContentID(target.ID, kind, id) })
				if err != nil {
					return nil, err
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(wrapped), &fields); err != nil {
					return nil, err
				}
				draft.PendingPlanImplementation = string(fields["sourceProposedPlan"])
			}
			// Captured snippets contain text, not live terminal handles.
			if draft.TerminalChips == "" {
				draft.TerminalChips = "[]"
			}
			if !json.Valid([]byte(draft.TerminalChips)) {
				return nil, errors.New("transfer: invalid terminal snippets")
			}
			hasContent := strings.TrimSpace(draft.Content) != "" || (draft.Attachments != "[]" && draft.Attachments != "null") || draft.PendingPlanImplementation != "" || (draft.TerminalChips != "[]" && draft.TerminalChips != "null")
			_, err := tx.Exec(`INSERT INTO thread_drafts (thread_id,content,attachments,terminal_chips,pending_plan_implementation,updated_at,has_content) VALUES (?,?,?,?,?,?,?)`, target.ID, draft.Content, draft.Attachments, draft.TerminalChips, nilIfEmpty(draft.PendingPlanImplementation), draft.UpdatedAt, boolToInt(hasContent))
			if err != nil {
				return nil, err
			}
		case "end":
			ended = true
		default:
			if err := importTransferAnnotation(tx, target.ID, sourceID, record); err != nil {
				return nil, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if limited.N == 0 {
		return nil, errors.New("transfer: conversation history exceeds the file limit")
	}
	if !ended {
		return nil, errors.New("transfer: incomplete conversation history")
	}
	if err := bumpHistoryRevTx(tx, target.ID, "transfer history"); err != nil {
		return nil, fmt.Errorf("transfer: initialize history: %w", err)
	}
	return tallies, nil
}

// A turn's durable ID is global in SQLite, but its provider ID is local to
// the native conversation. Rescope only AO identities on a copy; a move keeps
// them stable, including a later move of a copied conversation.
func transferredTurnID(sourceID, targetID, id string) string {
	if id == "" || sourceID == targetID {
		return id
	}
	return ScopedTurnID(targetID, strings.TrimPrefix(id, sourceID+":"), 0)
}

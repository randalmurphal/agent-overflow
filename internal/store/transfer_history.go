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

const transferHistoryVersion = 1
const transferHistoryChunk = 256 << 10
const transferHistoryRecordLimit = 32 << 20
const transferHistoryRowLimit = 2_000_000

type transferHistoryWriter struct {
	io.Writer
	remaining int64
}

func (w *transferHistoryWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("transfer: conversation history exceeds the file limit")
	}
	n, err := w.Writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}

// The portable history format is independent of SQLite schema versions. Records
// carry the ordinary typed data shapes; additive fields remain compatible. A
// new required record kind needs a format version and a compatibility gate.
type transferHistoryRecord struct {
	Kind    string          `json:"kind"`
	Version int             `json:"version,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type transferPayloadChunk struct {
	ID     string `json:"id"`
	Offset int    `json:"offset"`
	Data   []byte `json:"data"`
}

type transferPayloadMeta struct {
	PayloadMeta
	Size int `json:"size"`
}

func writeHistoryRecord(w io.Writer, kind string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > transferHistoryRecordLimit-256 {
		return errors.New("transfer: history metadata exceeds the record limit")
	}
	return json.NewEncoder(w).Encode(transferHistoryRecord{Kind: kind, Version: transferHistoryVersion, Data: data})
}

// ExportThreadHistory streams the LOGICAL timeline, including imported/shared
// history, subagent rows and append-backed payload bytes. Memory is bounded by
// one metadata page and one payload chunk. The caller must quiesce the provider
// and hold the thread action lock; neither a database transaction nor a lock is
// held over network I/O because this writes a private local snapshot first.
func (s *Store) ExportThreadHistory(ctx context.Context, threadID string, output io.Writer) error {
	return s.ExportThreadHistoryWith(ctx, threadID, output, ThreadHistoryExport{})
}

// ThreadHistoryExport lets the owning provider adapter rewrite structured
// native identities in the snapshot, without mutating the source history.
// Only metadata is exposed: native-reference rewrites cannot change AO row
// identity, ordering, or prose. Hydrated pages are in timeline order, which can
// differ from the ID order used to select each page.
type ThreadHistoryExport struct {
	ItemMeta func(string) (string, error)
}

func (s *Store) ExportThreadHistoryWith(ctx context.Context, threadID string, output io.Writer, transform ThreadHistoryExport) error {
	output = &transferHistoryWriter{Writer: output, remaining: transferfiles.MaxFileBytes}
	thread, err := s.GetThread(threadID)
	if err != nil {
		return err
	}
	if _, active, err := s.GetActiveTurn(threadID); err != nil {
		return err
	} else if active {
		return errors.New("transfer: wait for the conversation to finish before transferring it")
	}
	if err := writeHistoryRecord(output, "thread", thread); err != nil {
		return err
	}
	if err := s.exportTransferPayloads(ctx, threadID, output); err != nil {
		return err
	}
	attachments, err := s.ThreadTransferAttachments(ctx, threadID)
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if err := writeHistoryRecord(output, "attachment", attachment); err != nil {
			return err
		}
	}
	after := ""
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		items, err := queryHydratedTimelineItems(s.reader(), threadID, `SELECT id FROM timeline_items WHERE thread_id = ? AND id > ? ORDER BY id LIMIT 128`, threadID, after)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			count++
			if count > transferHistoryRowLimit {
				return errors.New("transfer: conversation exceeds the history row limit")
			}
			if item.ID > after {
				after = item.ID
			}
			item.PayloadPreviewSpans = ""
			if transform.ItemMeta != nil {
				item.Meta, err = transform.ItemMeta(item.Meta)
				if err != nil {
					return err
				}
			}
			if err := writeHistoryRecord(output, "item", item); err != nil {
				return err
			}
		}
	}
	if err := exportTransferRows(ctx, s.reader(), output, "turn", `SELECT `+turnColumns+` FROM turns WHERE thread_id = ? ORDER BY turn_index`, threadID, scanTurnRow); err != nil {
		return err
	}
	if err := exportTransferRows(ctx, s.reader(), output, "anchor", `SELECT `+messageAnchorColumns+` FROM message_anchors WHERE thread_id = ? ORDER BY turn_index, user_item_id`, threadID, scanMessageAnchor); err != nil {
		return err
	}
	if err := s.exportTransferAnnotations(ctx, threadID, output); err != nil {
		return err
	}
	if draft, found, err := s.GetThreadDraft(threadID); err != nil {
		return err
	} else if found {
		if err := writeHistoryRecord(output, "draft", draft); err != nil {
			return err
		}
	}
	return writeHistoryRecord(output, "end", nil)
}

func (s *Store) exportTransferPayloads(ctx context.Context, threadID string, output io.Writer) error {
	after := ""
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.reader().Query(`SELECT id, kind, meta, created_at FROM timeline_payloads WHERE thread_id = ? AND id > ? ORDER BY id LIMIT 128`, threadID, after)
		if err != nil {
			return err
		}
		var page []PayloadMeta
		for rows.Next() {
			var p PayloadMeta
			if err := rows.Scan(&p.ID, &p.Kind, &p.Meta, &p.CreatedAt); err != nil {
				rows.Close()
				return err
			}
			page = append(page, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, p := range page {
			count++
			if count > transferHistoryRowLimit {
				return errors.New("transfer: too many payloads")
			}
			after = p.ID
			data, total, done, err := s.GetPayloadChunk(threadID, p.ID, 0, transferHistoryChunk)
			if err != nil {
				return err
			}
			if err := writeHistoryRecord(output, "payload", transferPayloadMeta{PayloadMeta: p, Size: total}); err != nil {
				return err
			}
			for offset := 0; ; {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(data) != 0 {
					if err := writeHistoryRecord(output, "payload_chunk", transferPayloadChunk{ID: p.ID, Offset: offset, Data: data}); err != nil {
						return err
					}
					offset += len(data)
				}
				if done {
					break
				}
				if len(data) == 0 {
					return errors.New("transfer: payload read made no progress")
				}
				data, _, done, err = s.GetPayloadChunk(threadID, p.ID, offset, transferHistoryChunk)
				if err != nil {
					return err
				}
			}
		}
	}
}

func exportTransferRows[T any](ctx context.Context, q sqlQueryer, output io.Writer, kind, query, threadID string, scan func(interface{ Scan(...any) error }) (T, error)) error {
	rows, err := q.Query(query, threadID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > transferHistoryRowLimit {
			return errors.New("transfer: too many metadata rows")
		}
		value, err := scan(rows)
		if err != nil {
			return err
		}
		if err := writeHistoryRecord(output, kind, value); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ImportThreadHistory installs a validated local snapshot into a NEW thread in
// one transaction. The caller supplies destination workspace/provider settings;
// the archive cannot name another project's execution target. No credentials,
// host settings, usage ledger rows or derived rendering caches are imported.
func (s *Store) ImportThreadHistory(ctx context.Context, target Thread, input io.Reader) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := importThreadHistoryTx(ctx, tx, target, input); err != nil {
		return err
	}
	return tx.Commit()
}

func importThreadHistoryTx(ctx context.Context, tx *sql.Tx, target Thread, input io.Reader) error {
	prepared, lastReadAt, err := prepareThreadForCreate(target)
	if err != nil {
		return err
	}
	if err := insertThread(tx, prepared, lastReadAt); err != nil {
		return err
	}
	return readTransferHistoryTx(ctx, tx, target, input)
}

func readTransferHistoryTx(ctx context.Context, tx *sql.Tx, target Thread, input io.Reader) error {
	limited := &io.LimitedReader{R: input, N: transferfiles.MaxFileBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), transferHistoryRecordLimit)
	var sourceID string
	var err error
	attachments := make(map[string]itemmeta.AttachmentDestination)
	var payload transferPayloadMeta
	var offset, index int
	count := 0
	ended := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > transferHistoryRowLimit*4 {
			return errors.New("transfer: history exceeds the record limit")
		}
		if ended {
			return errors.New("transfer: data after history end marker")
		}
		var record transferHistoryRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return err
		}
		if record.Version != transferHistoryVersion {
			return errors.New("transfer: unsupported history format; update this computer first")
		}
		if count == 1 && record.Kind != "thread" {
			return errors.New("transfer: missing conversation header")
		}
		if record.Kind != "payload_chunk" && payload.ID != "" {
			if offset != payload.Size {
				return errors.New("transfer: incomplete payload data")
			}
			payload = transferPayloadMeta{}
		}
		switch record.Kind {
		case "thread":
			if count != 1 {
				return errors.New("transfer: duplicate conversation header")
			}
			var source Thread
			if err := json.Unmarshal(record.Data, &source); err != nil {
				return err
			}
			if source.ID == "" || source.Provider != target.Provider {
				return errors.New("transfer: conversation provider does not match the destination")
			}
			sourceID = source.ID
		case "payload":
			if err := json.Unmarshal(record.Data, &payload); err != nil {
				return err
			}
			if payload.ID == "" || payload.Size < 0 || int64(payload.Size) > 2<<30 {
				return errors.New("transfer: payload has no identity")
			}
			offset, index = 0, 0
			if err := insertPayloadTx(tx, target.ID, Payload{ID: payload.ID, Kind: payload.Kind, Meta: payload.Meta, Data: []byte{}, CreatedAt: payload.CreatedAt}, "transfer payload"); err != nil {
				return err
			}
		case "payload_chunk":
			var chunk transferPayloadChunk
			if err := json.Unmarshal(record.Data, &chunk); err != nil {
				return err
			}
			if payload.ID == "" || chunk.ID != payload.ID || chunk.Offset != offset || len(chunk.Data) == 0 || len(chunk.Data) > transferHistoryChunk || len(chunk.Data) > payload.Size-offset {
				return errors.New("transfer: payload chunks are missing, reordered or oversized")
			}
			_, err := tx.Exec(`INSERT INTO payload_chunks (thread_id,payload_id,chunk_index,start_offset,data,created_at) VALUES (?,?,?,?,?,?)`, target.ID, payload.ID, index, offset, chunk.Data, payload.CreatedAt)
			if err != nil {
				return err
			}
			offset += len(chunk.Data)
			index++
		case "item":
			var item Item
			if err := json.Unmarshal(record.Data, &item); err != nil {
				return err
			}
			if item.ThreadID != sourceID || item.ID == "" {
				return errors.New("transfer: item belongs to another conversation")
			}
			item.ThreadID, item.PayloadPreviewSpans = target.ID, ""
			if item.Kind == "user_text" {
				item.Meta, err = itemmeta.TransferAttachments(item.Meta, sourceID, attachments)
				if err != nil {
					return err
				}
				item.Meta, err = itemmeta.TransferThreadReferences(item.Meta, sourceID, target.ID, func(kind, id string) string { return transferContentID(target.ID, kind, id) })
				if err != nil {
					return err
				}
			}
			if err := insertItemTx(tx, item, "transfer item"); err != nil {
				return err
			}
		case "turn":
			var turn Turn
			if err := json.Unmarshal(record.Data, &turn); err != nil {
				return err
			}
			if turn.ThreadID != sourceID || turn.TurnID == "" || turn.CompletedAt == nil {
				return errors.New("transfer: turn is still running or belongs to another conversation")
			}
			turn.TurnID = transferredTurnID(sourceID, target.ID, turn.TurnID)
			_, err := tx.Exec(`INSERT INTO turns (`+turnColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`, turn.TurnID, target.ID, turn.TurnIndex, turn.StartedAt, turn.CompletedAt, turn.StopReason, turn.AssistantMessageID, turn.TokenUsageJSON, turn.ErrorMessage, turn.ProviderTurnID)
			if err != nil {
				return err
			}
		case "anchor":
			var anchor MessageAnchor
			if err := json.Unmarshal(record.Data, &anchor); err != nil {
				return err
			}
			if anchor.ThreadID != sourceID {
				return errors.New("transfer: message anchor belongs to another conversation")
			}
			_, err := tx.Exec(`INSERT INTO message_anchors (`+messageAnchorColumns+`) VALUES (?,?,?,?,?,?)`, target.ID, anchor.UserItemID, anchor.TurnIndex, anchor.ProviderUserMessageID, anchor.ProviderParentUUID, anchor.CreatedAt)
			if err != nil {
				return err
			}
		case "attachment":
			var a Attachment
			if err := json.Unmarshal(record.Data, &a); err != nil {
				return err
			}
			if a.ID == "" || (a.Kind != AttachmentKindImage && a.Kind != AttachmentKindFile) || len(attachments) >= 16_384 {
				return errors.New("transfer: invalid attachment metadata")
			}
			original := a
			a, err = TransferredAttachment(a, target.ID)
			if err != nil {
				return err
			}
			if _, exists := attachments[original.ID]; exists {
				return errors.New("transfer: duplicate attachment metadata")
			}
			attachments[original.ID] = itemmeta.AttachmentDestination{SourceThreadID: original.ThreadID, ThreadID: target.ID, ID: a.ID}
			if err := importTransferAttachment(tx, a); err != nil {
				return err
			}
		case "draft":
			var draft ThreadDraft
			if err := json.Unmarshal(record.Data, &draft); err != nil {
				return err
			}
			if draft.ThreadID != sourceID || !json.Valid([]byte(draft.Attachments)) {
				return errors.New("transfer: invalid composer draft")
			}
			draft.Attachments, err = itemmeta.TransferAttachmentArray(draft.Attachments, sourceID, attachments)
			if err != nil {
				return err
			}
			if draft.PendingPlanImplementation != "" {
				wrapped, err := itemmeta.TransferThreadReferences(`{"sourceProposedPlan":`+draft.PendingPlanImplementation+`}`, sourceID, target.ID, func(kind, id string) string { return transferContentID(target.ID, kind, id) })
				if err != nil {
					return err
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(wrapped), &fields); err != nil {
					return err
				}
				draft.PendingPlanImplementation = string(fields["sourceProposedPlan"])
			}
			// Terminal handles belong to processes on the source computer.
			hasContent := strings.TrimSpace(draft.Content) != "" || (draft.Attachments != "[]" && draft.Attachments != "null") || draft.PendingPlanImplementation != ""
			_, err := tx.Exec(`INSERT INTO thread_drafts (thread_id,content,attachments,terminal_chips,pending_plan_implementation,updated_at,has_content) VALUES (?,?,?,'[]',?,?,?)`, target.ID, draft.Content, draft.Attachments, nilIfEmpty(draft.PendingPlanImplementation), draft.UpdatedAt, boolToInt(hasContent))
			if err != nil {
				return err
			}
		case "end":
			ended = true
		default:
			if err := importTransferAnnotation(tx, target.ID, sourceID, record); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if limited.N == 0 {
		return errors.New("transfer: conversation history exceeds the file limit")
	}
	if !ended {
		return errors.New("transfer: incomplete conversation history")
	}
	if err := bumpHistoryRevTx(tx, target.ID, "transfer history"); err != nil {
		return fmt.Errorf("transfer: initialize history: %w", err)
	}
	return nil
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

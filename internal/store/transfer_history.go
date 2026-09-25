package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"

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
// A pointer fork's inherited rows are read where they are: the rows a fork
// shows never change (fork_triggers.go), so the export writes nothing.
func (s *Store) ExportThreadHistory(ctx context.Context, threadID string, output io.Writer) error {
	return s.ExportThreadHistoryWith(ctx, threadID, output, ThreadHistoryExport{})
}

// ThreadHistoryExport transforms native item metadata and the unsent draft
// without mutating the source. Hydrated pages are in timeline order, which can
// differ from the ID order used to select each page.
type ThreadHistoryExport struct {
	ItemMeta func(string) (string, error)
	Draft    func(ThreadDraft) (ThreadDraft, error)
}

func (s *Store) ExportThreadHistoryWith(ctx context.Context, threadID string, output io.Writer, transform ThreadHistoryExport) error {
	var recovery int
	if err := s.reader().QueryRow(`SELECT COUNT(*) FROM thread_draft_recoveries WHERE thread_id = ?`, threadID).Scan(&recovery); err != nil {
		return err
	}
	if recovery != 0 {
		return errors.New("transfer: an edited message is still awaiting recovery")
	}

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
	if err := exportTransferRows(ctx, s.reader(), output, "turn", `SELECT `+turnColumns+` FROM timeline_turns WHERE thread_id = ? ORDER BY turn_index`, threadID, scanTurnRow); err != nil {
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
		if transform.Draft != nil {
			draft, err = transform.Draft(draft)
			if err != nil {
				return err
			}
			if draft.ThreadID != threadID {
				return errors.New("transfer: draft transform changed thread identity")
			}
		}
		if err := writeHistoryRecord(output, "draft", draft); err != nil {
			return err
		}
	}
	return writeHistoryRecord(output, "end", nil)
}

// exportTransferPayloads writes the payloads the thread renders. A pointer
// fork's timeline_payloads resolve every payload its ancestors hold, past
// its cuts too, so a payload no row the fork shows names stays behind
// (transferPayloadRenderedSQL).
func (s *Store) exportTransferPayloads(ctx context.Context, threadID string, output io.Writer) error {
	after := ""
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.reader().Query(`SELECT id, kind, meta, created_at FROM timeline_payloads p
			 WHERE thread_id = ?1 AND id > ?2 AND `+transferPayloadRenderedSQL+` ORDER BY id LIMIT 128`, threadID, after)
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

// transferPayloadRenderedSQL is true for a timeline_payloads row p of
// thread ?1 that the thread holds itself, or that a row it shows through
// its lineage names (inheritedPayloadRowArms, keyed by the payload).
var transferPayloadRenderedSQL = `(` + payloadHeldBySQL("?1", "p.id") + `
	OR EXISTS (` + inheritedPayloadRowArms("1", allLevels, "?1", "p.id") + `))`

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

package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Blanking legacy transcript copies is the second step of migration v119's
// deferred phase (blankLegacyTranscriptCopies).
//
// A Claude background agent's output file is its sidechain transcript.
// Current builds project the transcript's rows into the thread and write the
// completion's payload with empty data, keeping the output file, its
// outputFileState "loaded" and the report preview in meta
// (triage.buildBackgroundOutputFilePayload). Earlier builds wrote the same
// payload with the whole transcript file as its data. The step empties the
// data of those payloads and keeps the row, its kind, meta, preview spans and
// creation time, so a legacy payload reads as a current one. Highlight spans
// describe the data, so they are cleared with it. Item rows and their
// revisions are not touched.
//
// A payload qualifies through the completion row that names it: a
// background tool_completion of Agent, Task or SendMessage in a Claude
// thread. A payload no completion names, a Monitor or command completion's
// output, and a payload without outputFileState "loaded" keep their data.

// legacyTranscriptCompletionSQL is true when a completion row c of a Claude
// thread names payload (thread, id) as a background agent's result.
func legacyTranscriptCompletionSQL(thread, payload string) string {
	return `EXISTS (SELECT 1 FROM items c JOIN threads t ON t.id = c.thread_id
	 WHERE c.thread_id = ` + thread + ` AND c.payload_id IS NOT NULL AND c.payload_id = ` + payload + `
	   AND c.completion_of <> '' AND c.kind = 'tool_completion' AND c.is_background = 1
	   AND c.tool_name IN ('Agent', 'Task', 'SendMessage') AND t.provider = 'claude')`
}

// legacyTranscriptPayloadSQL is true when payload row p still holds a
// transcript copy.
const legacyTranscriptPayloadSQL = `p.kind = 'tool_call_result' AND length(p.data) > 0
 AND CASE WHEN json_valid(p.meta) THEN json_extract(p.meta, '$.outputFileState') END = 'loaded'`

// transcriptBlankRows bounds the payloads one blanking transaction empties;
// historyRepairBytes bounds their bytes. Emptying a payload frees its
// overflow pages, so a transaction's cost follows its bytes; on the measured
// database a 4 MiB transaction took at most 18 ms.
const transcriptBlankRows = 64

type legacyTranscriptPayload struct {
	threadID string
	id       string
	bytes    int64
}

// blankLegacyTranscriptCopies is the second step of migration v119's
// deferred phase. It empties the data of legacy transcript copies in
// transactions of at most transcriptBlankRows payloads and
// historyRepairBytes bytes, each followed by a checkpoint, pausing between
// them. Each update re-checks the selection inside its transaction. A batch
// whose update fails is reported to run and left as it is.
func blankLegacyTranscriptCopies(ctx context.Context, s *Store, run *deferredRun) error {
	start := time.Now()
	payloads, err := s.legacyTranscriptPayloads(ctx)
	if err != nil {
		return err
	}
	blanked, freed := 0, int64(0)
	for begin := 0; begin < len(payloads); {
		if ctx.Err() != nil {
			return nil
		}
		end, bytes := begin, int64(0)
		for end < len(payloads) && end-begin < transcriptBlankRows && (end == begin || bytes+payloads[end].bytes <= historyRepairBytes) {
			bytes += payloads[end].bytes
			end++
		}
		if begin > 0 {
			run.pause()
			if ctx.Err() != nil {
				return nil
			}
		}
		batch := payloads[begin:end]
		begin = end
		count, batchBytes, err := s.blankLegacyTranscriptBatch(batch)
		if err != nil {
			run.fail(fmt.Errorf("transcript blank: %d payloads from %s/%s left in place: %w", len(batch), batch[0].threadID, batch[0].id, err))
			continue
		}
		blanked += count
		freed += batchBytes
		if count > 0 {
			if err := s.checkpointHistoryRepair(); err != nil {
				return err
			}
		}
	}
	log.Printf("store: transcript blank: emptied %d legacy transcript copies (%d bytes) in %s",
		blanked, freed, time.Since(start).Round(time.Millisecond))
	return nil
}

// legacyTranscriptPayloads lists the payloads that still hold a transcript
// copy, in primary-key order.
func (s *Store) legacyTranscriptPayloads(ctx context.Context) ([]legacyTranscriptPayload, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT p.thread_id, p.id, length(p.data) FROM payloads p
 WHERE `+legacyTranscriptPayloadSQL+`
   AND `+legacyTranscriptCompletionSQL("p.thread_id", "p.id")+`
 ORDER BY p.thread_id, p.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list legacy transcript copies: %w", err)
	}
	var payloads []legacyTranscriptPayload
	for rows.Next() {
		var payload legacyTranscriptPayload
		if err := rows.Scan(&payload.threadID, &payload.id, &payload.bytes); err != nil {
			return nil, errors.Join(fmt.Errorf("store: read legacy transcript copy: %w", err), rows.Close())
		}
		payloads = append(payloads, payload)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list legacy transcript copies: %w", err)
	}
	return payloads, nil
}

// blankLegacyTranscriptBatch empties the batch's payloads that still hold a
// transcript copy, in one transaction. It returns how many it emptied and
// their bytes.
func (s *Store) blankLegacyTranscriptBatch(batch []legacyTranscriptPayload) (int, int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("store: begin transcript blank: %w", err)
	}
	defer tx.Rollback()
	count, bytes := 0, int64(0)
	for _, payload := range batch {
		result, err := tx.Exec(`UPDATE payloads AS p SET data = X'', spans = ''
 WHERE p.thread_id = ? AND p.id = ? AND `+legacyTranscriptPayloadSQL+`
   AND `+legacyTranscriptCompletionSQL("p.thread_id", "p.id"), payload.threadID, payload.id)
		if err != nil {
			return 0, 0, fmt.Errorf("store: empty transcript copy %s/%s: %w", payload.threadID, payload.id, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return 0, 0, fmt.Errorf("store: count emptied transcript copy %s/%s: %w", payload.threadID, payload.id, err)
		}
		if updated > 0 {
			count++
			bytes += payload.bytes
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: commit transcript blank: %w", err)
	}
	return count, bytes, nil
}

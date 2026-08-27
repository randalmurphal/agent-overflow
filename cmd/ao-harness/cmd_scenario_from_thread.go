package main

// `scenario from-thread`: the state-clone repro rig's second half. It
// reads real recorded turns out of the TARGET INSTANCE's store and writes
// them back out as a mock-provider scenario, so "reproduce what my app
// feels like" is a command rather than an afternoon of hand-written
// fixtures.
//
// The read is the same read-only discipline `db` uses (mode=ro plus
// query_only), against the path the instance itself reports. Nothing here
// writes to the store.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/harnessclient"
)

// defaultFromThreadDelayMs paces the replay. The recorded chunk
// boundaries carry the SHAPE of the original stream; they carry no
// timing, because the store stamps writes rather than arrivals. So the
// cadence is a knob, and its default is "readable" rather than "as fast
// as the pipe accepts", which is what makes a replayed turn look like a
// turn instead of a paste.
const defaultFromThreadDelayMs = 15

func scenarioFromThread(e *env, args []string) error {
	flags := e.newFlagSet("scenario from-thread")
	threadSel := flags.String("thread", "", "thread to rebuild (id, #N, `last`, or a title prefix)")
	turns := flags.Int("turns", 1, "how many of the thread's most recent turns to rebuild")
	out := flags.String("out", "", "write the scenario document here (default: stdout)")
	set := flags.Bool("set", false, "also install the document on the instance as an inline scenario rule")
	delayMs := flags.Int("delay-ms", defaultFromThreadDelayMs, "milliseconds between wire lines inside a burst")
	cadence := flags.String("cadence", "real", "replay pacing: `real` replays the rows' recorded gaps (capped by --gap-cap-ms), `uniform` streams every line --delay-ms apart")
	gapCapMs := flags.Int("gap-cap-ms", defaultGapCapMs, "real cadence only: cap one recorded gap at this many milliseconds")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("scenario from-thread takes no positional arguments (got %v)", rest)
	}
	if strings.TrimSpace(*threadSel) == "" {
		return usagef("scenario from-thread needs --thread (an id, #N, `last`, or a title prefix)")
	}
	if *turns < 1 {
		return usagef("--turns must be at least 1 (got %d)", *turns)
	}
	if *delayMs < 0 {
		return usagef("--delay-ms must not be negative (got %d)", *delayMs)
	}
	if *cadence != "real" && *cadence != "uniform" {
		return usagef("--cadence must be `real` or `uniform` (got %q)", *cadence)
	}
	if *gapCapMs < 1 {
		return usagef("--gap-cap-ms must be at least 1 (got %d)", *gapCapMs)
	}

	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		row, err := resolveThreadSelector(ctx, client, *threadSel)
		if err != nil {
			return err
		}
		info, err := client.Info(ctx)
		if err != nil {
			return err
		}
		if info.DBPath == "" {
			return fmt.Errorf("the instance reported no database path")
		}
		db, err := openReadOnly(info.DBPath)
		if err != nil {
			return err
		}
		defer db.Close()

		recorded, err := readRecordedTurns(db, row.ID, *turns)
		if err != nil {
			return err
		}
		if len(recorded) == 0 {
			return fmt.Errorf("thread %s has no recorded turns to rebuild", row.ID)
		}

		doc, stats, err := synthesizeScenario(synthOptions{
			Name:        scenarioNameForThread(row),
			Provider:    row.Provider,
			ThreadID:    row.ID,
			Title:       row.Title,
			Turns:       recorded,
			DelayMs:     *delayMs,
			CadenceReal: *cadence == "real",
			GapCapMs:    *gapCapMs,
		})
		if err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("encode scenario: %w", err)
		}

		written := ""
		if strings.TrimSpace(*out) != "" {
			written, err = writeScenarioFile(*out, encoded)
			if err != nil {
				return err
			}
		}
		installed := false
		if *set {
			if err := installScenarioDocument(ctx, client, encoded); err != nil {
				return err
			}
			installed = true
		}

		if e.jsonOutput() {
			return e.writeJSON(map[string]any{
				"thread":    row.ID,
				"provider":  row.Provider,
				"turns":     turnIndexes(recorded),
				"stats":     stats,
				"out":       written,
				"installed": installed,
				"scenario":  json.RawMessage(encoded),
				"sends":     sendRecipe(row.ID, recorded),
			})
		}
		if written == "" && !*set {
			// stdout IS the output when no destination was named, so the
			// report goes to stderr rather than into the document.
			e.printf("%s\n", encoded)
		}
		return e.reportFromThread(row, recorded, stats, doc.Name, written, installed)
	})
}

// scenarioNameForThread keeps the name short, stable and obviously
// derived, so two rebuilds of the same thread overwrite one rule rather
// than accumulating rules that differ only by timestamp.
func scenarioNameForThread(row threadRow) string {
	id := row.ID
	if len(id) > 8 {
		id = id[:8]
	}
	return "from-thread-" + id
}

func turnIndexes(turns []recordedTurn) []int {
	out := make([]int, 0, len(turns))
	for _, turn := range turns {
		out = append(out, turn.TurnIndex)
	}
	return out
}

func writeScenarioFile(path string, encoded []byte) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve --out %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(abs), err)
	}
	// 0600: the document embeds real recorded conversation content.
	if err := os.WriteFile(abs, append(encoded, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", abs, err)
	}
	return abs, nil
}

func installScenarioDocument(ctx context.Context, client *harnessclient.Client, encoded []byte) error {
	_, err := client.Call(ctx, "HarnessSetScenario", map[string]any{"scenario": json.RawMessage(encoded)})
	return err
}

// sendRecipe is the drive half: one `send` per rebuilt turn, carrying the
// user text that produced it. The scenario replays assistant work only —
// a real send is what opens each Turn — so without these lines the
// document is a script with no trigger.
func sendRecipe(threadID string, turns []recordedTurn) []string {
	out := make([]string, 0, len(turns))
	for _, turn := range turns {
		out = append(out, fmt.Sprintf("ao-harness send --thread %s --wait %s", threadID, shellQuote(turn.UserText)))
	}
	return out
}

func (e *env) reportFromThread(row threadRow, turns []recordedTurn, stats synthStats, name, written string, installed bool) error {
	e.printf("scenario %s (%s)\n", name, row.Provider)
	e.printf("  thread    %s  %s\n", row.ID, truncate(row.Title, 48))
	e.printf("  turns     %d (turn_index %v)\n", stats.Turns, turnIndexes(turns))
	e.printf("  items     %d replayed", stats.Items)
	if summary := stats.SkippedSummary(); summary != "" {
		e.printf(", skipped %s", summary)
	}
	e.printf("\n")
	e.printf("  deltas    %d\n", stats.Deltas)
	if stats.Empty > 0 {
		e.printf("  empty     %d item(s) had no stored payload; they replay with empty content\n", stats.Empty)
	}
	if written != "" {
		e.printf("  written   %s\n", written)
	}
	if installed {
		e.printf("  installed inline on this instance (`ao-harness scenario list` shows the rule)\n")
	}
	e.printf("\ndrive it:\n")
	if written != "" && !installed {
		e.printf("  ao-harness scenario set -f %s\n", written)
	}
	for _, line := range sendRecipe(row.ID, turns) {
		e.printf("  %s\n", line)
	}
	e.printf("\nthis scenario embeds real recorded session content — do not commit it.\n")
	return nil
}

// -- store reads ----------------------------------------------------------
//
// Every statement filters thread_id, payload reads included. `payloads`
// has been keyed (thread_id, id) since migration v58 precisely because
// item and payload ids repeat across threads — Claude sibling branches
// share their prefix and live thinking coordinates repeat everywhere — so
// a payload read scoped by id alone can and does return another thread's
// bytes. (frontend/scripts/generate-freeze-replay-fixture.mjs predates
// that migration and still has the bug; it is not a model to copy.)
//
// Items and payloads are read through `timeline_items` / `timeline_payloads`,
// the logical-history views (migration v61), not the base tables: an
// IMPORTED thread's rows live in the shared chunk tables, and reading
// `items` directly would rebuild a real imported thread as an empty one.

// readRecordedTurns loads the thread's last `want` turns.
func readRecordedTurns(db *sql.DB, threadID string, want int) ([]recordedTurn, error) {
	indexes, err := readTurnIndexes(db, threadID)
	if err != nil {
		return nil, err
	}
	if len(indexes) == 0 {
		return nil, nil
	}
	if want < len(indexes) {
		indexes = indexes[len(indexes)-want:]
	}
	first := indexes[0]

	// Two passes on purpose: the CLI's read-only handle is capped at ONE
	// connection (openReadOnlyDB, the same discipline `db` runs under),
	// and an open rows cursor HOLDS it. A payload read issued inside the
	// scan loop parks in database/sql's connection wait forever — observed
	// live 2026-08-26 as a silent from-thread hang. So: scan every item
	// row and close the cursor, then load payloads on the freed connection.
	rows, err := db.Query(
		`SELECT id, turn_index, item_index, kind, role, tool_name, completion_of, summary,
		        COALESCE(status, ''), COALESCE(created_at, 0), COALESCE(updated_at, 0),
		        COALESCE(payload_id, ''), COALESCE(input_payload_id, '')
		   FROM timeline_items
		  WHERE thread_id = ? AND turn_index >= ?
		  ORDER BY turn_index, item_index`,
		threadID, first,
	)
	if err != nil {
		return nil, fmt.Errorf("read items for thread %s: %w", threadID, err)
	}
	type scannedItem struct {
		item      recordedItem
		payloadID string
		inputID   string
	}
	var scanned []scannedItem
	for rows.Next() {
		var row scannedItem
		if err := rows.Scan(&row.item.ID, &row.item.TurnIndex, &row.item.ItemIndex, &row.item.Kind, &row.item.Role,
			&row.item.ToolName, &row.item.CompletionOf, &row.item.Summary,
			&row.item.Status, &row.item.CreatedAtMs, &row.item.UpdatedAtMs, &row.payloadID, &row.inputID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read items for thread %s: %w", threadID, err)
		}
		scanned = append(scanned, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read items for thread %s: %w", threadID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read items for thread %s: %w", threadID, err)
	}

	byTurn := map[int]*recordedTurn{}
	var order []int
	for i := range scanned {
		item := scanned[i].item
		if scanned[i].payloadID != "" {
			item.Pieces, err = readPayloadPieces(db, threadID, scanned[i].payloadID)
			if err != nil {
				return nil, err
			}
		}
		if scanned[i].inputID != "" {
			pieces, err := readPayloadPieces(db, threadID, scanned[i].inputID)
			if err != nil {
				return nil, err
			}
			var b strings.Builder
			for _, p := range pieces {
				b.WriteString(p.Text)
			}
			item.Input = b.String()
		}
		turn, seen := byTurn[item.TurnIndex]
		if !seen {
			turn = &recordedTurn{TurnIndex: item.TurnIndex}
			byTurn[item.TurnIndex] = turn
			order = append(order, item.TurnIndex)
		}
		if item.Kind == kindUserText && turn.UserText == "" {
			turn.UserText = firstNonEmpty(item.Content(), item.Summary)
		}
		turn.Items = append(turn.Items, item)
	}

	out := make([]recordedTurn, 0, len(order))
	for _, index := range order {
		out = append(out, *byTurn[index])
	}
	return out, nil
}

// readTurnIndexes lists the turn positions the thread actually holds
// items at. It reads the ITEMS rather than the `turns` table on purpose:
// a turn row with no items rebuilds as an empty Turn, and the window the
// caller asked for is "the last N turns that produced something".
func readTurnIndexes(db *sql.DB, threadID string) ([]int, error) {
	rows, err := db.Query(
		`SELECT DISTINCT turn_index FROM timeline_items WHERE thread_id = ? ORDER BY turn_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("read turn indexes for thread %s: %w", threadID, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, fmt.Errorf("read turn indexes for thread %s: %w", threadID, err)
		}
		out = append(out, index)
	}
	return out, rows.Err()
}

// readPayloadPieces returns the payload's content boundaries: the head
// followed by each appended chunk, in order. That is the delta set the
// stream was written at — appendPayloadDataTx writes one chunk per
// flushed delta — so replaying one line per piece reproduces the original
// boundaries rather than a re-chunking of the final text. Each piece
// carries its row's created_at (the head carries the payload row's), the
// stamps real-cadence replay is laid out on; an older row with no stamp
// reads as 0 and the synthesizer falls back to its neighbors.
//
// A payload row that is gone is not an error. Payload GC is real, and a
// missing blob means the item replays with empty content, which the
// caller counts and reports.
func readPayloadPieces(db *sql.DB, threadID, payloadID string) ([]recordedPiece, error) {
	var head []byte
	var headAt int64
	err := db.QueryRow(
		`SELECT data, COALESCE(created_at, 0) FROM timeline_payloads WHERE thread_id = ? AND id = ?`, threadID, payloadID,
	).Scan(&head, &headAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read payload %s of thread %s: %w", payloadID, threadID, err)
	}

	rows, err := db.Query(
		`SELECT data, COALESCE(created_at, 0) FROM payload_chunks
		  WHERE thread_id = ? AND payload_id = ?
		  ORDER BY chunk_index`,
		threadID, payloadID,
	)
	if err != nil {
		return nil, fmt.Errorf("read payload chunks %s of thread %s: %w", payloadID, threadID, err)
	}
	defer rows.Close()

	var pieces []recordedPiece
	if len(head) > 0 {
		pieces = append(pieces, recordedPiece{Text: string(head), AtMs: headAt})
	}
	for rows.Next() {
		var chunk []byte
		var at int64
		if err := rows.Scan(&chunk, &at); err != nil {
			return nil, fmt.Errorf("read payload chunks %s of thread %s: %w", payloadID, threadID, err)
		}
		pieces = append(pieces, recordedPiece{Text: string(chunk), AtMs: at})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read payload chunks %s of thread %s: %w", payloadID, threadID, err)
	}
	return pieces, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

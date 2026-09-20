package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

func threadToolsOS() string { return runtime.GOOS }

// The store's timeline coordinate is the pair (turn_index, item_index);
// threadtools wants one comparable int64 position. A position packs the
// pair into a single ascending number:
//
//	position = turn_index*turnPositionSpan + item_index + itemIndexBias
//
// turnPositionSpan is the per-turn slot count. itemIndexBias exists
// because item_index can be negative: a head-healed prompt persists below
// zero (UpsertItemAtTurnHead), and the bias keeps every real position
// positive, which the renderer's "after > 0" cursor check relies on.
const (
	turnPositionSpan = int64(1) << 20
	itemIndexBias    = int64(1) << 19
)

func encodePosition(turnIndex, itemIndex int) int64 {
	return int64(turnIndex)*turnPositionSpan + int64(itemIndex) + itemIndexBias
}

func decodePosition(position int64) (turnIndex, itemIndex int) {
	turn := position / turnPositionSpan
	rest := position - turn*turnPositionSpan
	return int(turn), int(rest - itemIndexBias)
}

// turnFloor and turnCeil are the first and last position a turn owns.
// They are real positions in the ordering whether or not a row sits
// exactly there, which is what lets a turn-granular window be expressed
// without reading the turn's rows.
func turnFloor(turnIndex int) int64 { return int64(turnIndex) * turnPositionSpan }
func turnCeil(turnIndex int) int64  { return int64(turnIndex)*turnPositionSpan + turnPositionSpan - 1 }

func itemPosition(item store.Item) int64 { return encodePosition(item.TurnIndex, item.ItemIndex) }

func cursorPosition(cursor store.TimelineCursor) int64 {
	return encodePosition(cursor.TurnIndex, cursor.ItemIndex)
}

// positionCursor is the inverse: the store coordinate a transport
// position names. The pair is the store's own timeline coordinate, so a
// position decodes into a bound a range query compares directly and no
// caller ever walks turns to find it.
func positionCursor(position int64) store.TimelineCursor {
	turn, item := decodePosition(position)
	return store.TimelineCursor{TurnIndex: turn, ItemIndex: item}
}

// ResolveWindow turns a requested window into absolute positions.
func (t threadToolsApp) ResolveWindow(_ context.Context, q threadtools.WindowQuery) (threadtools.WindowBounds, error) {
	thread, err := t.localThread(q.ThreadID)
	if err != nil {
		return threadtools.WindowBounds{}, err
	}
	// IsDraft is the store's own "this thread has no timeline row", which
	// is exactly the empty window and costs no extra probe.
	if thread.IsDraft {
		return threadtools.WindowBounds{Empty: true}, nil
	}
	// One indexed probe per edge. A thread whose newest turn has no item
	// yet, like one with nothing stored at all, has no window.
	first, last, ok, err := t.app.store.ThreadTimelineBounds(q.ThreadID)
	if err != nil {
		return threadtools.WindowBounds{}, err
	}
	if !ok {
		return threadtools.WindowBounds{Empty: true}, nil
	}
	bounds := threadtools.WindowBounds{From: cursorPosition(first), To: cursorPosition(last), HighWater: cursorPosition(last)}
	firstTurn, lastTurn := first.TurnIndex, last.TurnIndex
	turns := q.Turns
	switch q.Kind {
	case threadtools.WindowAll, "":
	case threadtools.WindowTail:
		if turns <= 0 {
			turns = threadtools.DefaultTailTurns
		}
		if start := lastTurn - turns + 1; start > firstTurn {
			bounds.From = turnFloor(start)
		}
	case threadtools.WindowHead:
		if turns <= 0 {
			turns = threadtools.DefaultTailTurns
		}
		if end := firstTurn + turns - 1; end < lastTurn {
			bounds.To = turnCeil(end)
		}
	case threadtools.WindowSince:
		from, empty, err := t.sinceFrom(q, firstTurn)
		if err != nil {
			return threadtools.WindowBounds{}, err
		}
		if empty || from > bounds.To {
			return threadtools.WindowBounds{Empty: true}, nil
		}
		if from > bounds.From {
			bounds.From = from
		}
	case threadtools.WindowAround:
		anchor, err := t.itemRow(q.ThreadID, q.ItemID)
		if err != nil {
			return threadtools.WindowBounds{}, err
		}
		if low := anchor.TurnIndex - turns; low > firstTurn {
			bounds.From = turnFloor(low)
		}
		if high := anchor.TurnIndex + turns; high < lastTurn {
			bounds.To = turnCeil(high)
		}
	default:
		return threadtools.WindowBounds{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("There is no window named %q.", q.Kind), nil)
	}
	return bounds, nil
}

// sinceFrom resolves the `since` anchor. An item anchor starts at the row
// after it. A timestamp anchor starts at the first turn that began at or
// after it, because a turn is the unit this window counts in.
func (t threadToolsApp) sinceFrom(q threadtools.WindowQuery, firstTurn int) (int64, bool, error) {
	if strings.TrimSpace(q.ItemID) != "" {
		anchor, err := t.itemRow(q.ThreadID, q.ItemID)
		if err != nil {
			return 0, false, err
		}
		return itemPosition(anchor) + 1, false, nil
	}
	if q.SinceUnixMs <= 0 {
		return turnFloor(firstTurn), false, nil
	}
	start, found, anyTurns, err := t.app.store.FirstTurnIndexAtOrAfter(q.ThreadID, q.SinceUnixMs)
	if err != nil {
		return 0, false, err
	}
	if !found {
		// Nothing started at or after it. A thread with turns has simply
		// been quiet since; one with no turn rows has no anchor to
		// resolve against and keeps the whole window.
		if anyTurns {
			return 0, true, nil
		}
		return turnFloor(firstTurn), false, nil
	}
	return turnFloor(start), false, nil
}

func (t threadToolsApp) itemRow(threadID, itemID string) (store.Item, error) {
	item, found, err := t.app.store.GetThreadItem(threadID, strings.TrimSpace(itemID))
	if err != nil {
		return store.Item{}, err
	}
	if !found {
		return store.Item{}, errorsx.Public(threadtools.CodeNotFound,
			fmt.Sprintf("No item %s in thread %s.", itemID, threadID), nil)
	}
	return item, nil
}

// Transcript returns one page of the position range, oldest first.
//
// The range is a store query, not a walk: the transport's packed
// positions decode into the (turn_index, item_index) pair the store
// orders by, and the page's limit is the query's LIMIT. One call is one
// round trip whatever the thread holds between the bounds.
//
// The include list decides whether subagent children are part of the
// range at all, so every row the store returns is a row this page ships
// and the limit cannot be spent on rows nobody asked for.
func (t threadToolsApp) Transcript(ctx context.Context, q threadtools.TranscriptQuery) (threadtools.TranscriptSlice, error) {
	if _, err := t.localThread(q.ThreadID); err != nil {
		return threadtools.TranscriptSlice{}, err
	}
	_, _, ok, err := t.app.store.ThreadTimelineBounds(q.ThreadID)
	if err != nil {
		return threadtools.TranscriptSlice{}, err
	}
	slice := threadtools.TranscriptSlice{Items: []threadtools.Item{}}
	if !ok {
		return slice, nil
	}
	items, err := t.transcriptPage(ctx, q)
	if err != nil {
		return threadtools.TranscriptSlice{}, err
	}
	slice.Items = append(slice.Items, items...)
	return slice, nil
}

// transcriptPage reads one page of a position range, oldest first.
//
// Resolving the thread and its bounds belongs to the caller: Transcript
// answers one page and does it per call, while an export walks a whole
// window and resolves once for the file.
func (t threadToolsApp) transcriptPage(ctx context.Context, q threadtools.TranscriptQuery) ([]threadtools.Item, error) {
	if q.Limit <= 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := t.app.store.ListItemsInRange(q.ThreadID, positionCursor(q.From), positionCursor(q.To), q.Limit,
		threadToolsIncludes(q.Include, "subagent"))
	if err != nil {
		return nil, err
	}
	items := make([]threadtools.Item, 0, len(rows))
	for _, row := range rows {
		item, err := t.projectItem(q, row, itemPosition(row))
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// projectItem turns one store row into a transcript row. A subagent child
// the caller did not ask for never reaches here: those rows are not part
// of any timeline window in this app, and listing them as bodiless
// one-liners would spend the whole page on them, so the range query
// leaves them out.
func (t threadToolsApp) projectItem(q threadtools.TranscriptQuery, row store.Item, position int64) (threadtools.Item, error) {
	kind, role := threadToolsItemKind(row)
	item := threadtools.Item{
		ID:       row.ID,
		Position: position,
		Kind:     kind,
		Role:     role,
		TurnID:   strconv.Itoa(row.TurnIndex),
		Name:     row.ToolName,
	}
	// A row the include list leaves out states its size and carries no
	// body, so it asks for the size alone and never reads the payload.
	maxBytes := 0
	if threadToolsIncludes(q.Include, kind) {
		maxBytes = q.MaxItemBytes
		if maxBytes <= 0 {
			maxBytes = threadItemWholeBody
		}
	}
	text, size, err := t.itemBody(row, maxBytes)
	if err != nil {
		return threadtools.Item{}, err
	}
	item.Size = size
	if maxBytes == 0 || size == 0 {
		return item, nil
	}
	item.Text = text
	item.Clipped = int64(len(text)) < size
	return item, nil
}

// threadToolsItemKind maps a store row onto the transcript vocabulary and
// the role its line is prefixed with.
func threadToolsItemKind(row store.Item) (kind, role string) {
	if row.ParentID != "" {
		return "subagent", "assistant"
	}
	switch row.Kind {
	case "user_text":
		return "user_text", "user"
	case "assistant_text":
		return "assistant_text", "assistant"
	case "thinking", "compaction_reasoning":
		return "thinking", "assistant"
	case "tool_call", "tool_completion", "command_result":
		if row.PayloadKind == "diff" {
			return "diff", "tool"
		}
		return "tool_call", "tool"
	case "error", "api_error", "api_retry":
		return "error", "system"
	}
	return row.Kind, "system"
}

// threadToolsIncludes mirrors the App contract: prose rows always carry
// their body, everything else only when the include list asks.
func threadToolsIncludes(include []string, kind string) bool {
	switch kind {
	case "user_text", "assistant_text", "error":
		return true
	}
	if slices.Contains(include, threadtools.IncludeAll) {
		return true
	}
	switch kind {
	case "thinking":
		return slices.Contains(include, threadtools.IncludeThinking)
	case "tool_call", "tool_output":
		return slices.Contains(include, threadtools.IncludeToolOutputs)
	case "diff":
		return slices.Contains(include, threadtools.IncludeDiffs)
	case "subagent":
		return slices.Contains(include, threadtools.IncludeSubagents)
	}
	return false
}

// threadItemWholeBody asks itemBody for everything the row holds. The
// store clips the range it reads to the payload's own length, so this
// stands in for a size the caller has not paid to learn yet.
const threadItemWholeBody = 1<<31 - 1

// itemBody reads one row's body and reports its whole stored size: the
// linked payload when there is one, otherwise the summary the row carries
// inline. maxBytes clips what comes back, and zero asks for the size
// alone; one store read answers both, which is what keeps a transcript
// page to one read per item that has a payload.
func (t threadToolsApp) itemBody(row store.Item, maxBytes int) (string, int64, error) {
	data, size, err := t.itemBodyRange(row, 0, maxBytes)
	if err != nil {
		return "", 0, err
	}
	return string(data), size, nil
}

// itemBodyRange reads a byte range of a row a caller already holds, and
// reports the whole stored size. An offset at or past the end is an empty
// read with the size, never an error.
func (t threadToolsApp) itemBodyRange(row store.Item, offset int64, maxBytes int) ([]byte, int64, error) {
	if offset < 0 {
		offset = 0
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	if row.PayloadID == "" {
		size := int64(len(row.Summary))
		if maxBytes == 0 || offset >= size {
			return nil, size, nil
		}
		end := offset + int64(maxBytes)
		if end > size {
			end = size
		}
		return []byte(row.Summary[offset:end]), size, nil
	}
	data, total, _, err := t.app.store.GetPayloadChunk(row.ThreadID, row.PayloadID, int(offset), maxBytes)
	if err != nil {
		return nil, 0, err
	}
	return data, int64(total), nil
}

// ItemPayload reads a byte range of one item's stored body. An offset at
// or past the end is an empty read reporting the size.
func (t threadToolsApp) ItemPayload(ctx context.Context, q threadtools.PayloadQuery) (threadtools.Payload, error) {
	// A large item is streamed through repeated calls, so a cancelled
	// read must stop here rather than at the end of the item.
	if err := ctx.Err(); err != nil {
		return threadtools.Payload{}, err
	}
	if _, err := t.localThread(q.ThreadID); err != nil {
		return threadtools.Payload{}, err
	}
	row, err := t.itemRow(q.ThreadID, q.ItemID)
	if err != nil {
		return threadtools.Payload{}, err
	}
	kind, _ := threadToolsItemKind(row)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	// One read answers the size and the range alike, including the empty
	// read an offset past the end gets.
	data, size, err := t.itemBodyRange(row, offset, int(q.MaxBytes))
	if err != nil {
		return threadtools.Payload{}, err
	}
	return threadtools.Payload{Kind: kind, Size: size, Offset: offset, Bytes: data}, nil
}

// threadExportDirName is the directory under the app's data directory
// where rendered transcripts land. Files are retained until removed.
const threadExportDirName = "thread-exports"

// ExportTranscript renders a whole window to a file and reports its path,
// size and digest. It streams the window through the same turn walk the
// transcript uses, so a long thread never lands in memory whole.
func (t threadToolsApp) ExportTranscript(ctx context.Context, q threadtools.ExportQuery) (threadtools.ExportFile, error) {
	thread, err := t.localThread(q.ThreadID)
	if err != nil {
		return threadtools.ExportFile{}, err
	}
	if t.app.configDir == "" {
		return threadtools.ExportFile{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"This computer has no data directory configured, so a transcript cannot be exported to a file.", nil)
	}
	dir := filepath.Join(t.app.configDir, threadExportDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return threadtools.ExportFile{}, fmt.Errorf("thread tools: create export directory: %w", err)
	}
	// Every export carries a nonce, and an export a paired computer asked
	// for carries the peer prefix as well: the model holds the path it was
	// given for as long as it likes, so a second render of another window
	// must never land on the file the first one named. A remote export is
	// reported without this computer's path, because the model that reads
	// it is on the other computer and gets a path in its own export
	// directory once the copy lands.
	name := thread.ID + "." + entityid.New() + ".txt"
	exportID := ""
	if threadtools.Forwarded(ctx) {
		exportID = threadPeerExportName(thread.ID)
		name = exportID
	}
	path := filepath.Join(dir, name)
	// Rendered beside the final name and renamed onto it, so nothing ever
	// reads or fetches a half-written window.
	temp := path + ".partial"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return threadtools.ExportFile{}, fmt.Errorf("thread tools: open export file: %w", err)
	}
	// An export for another computer is bounded as it grows, not measured
	// afterwards: a window no transfer could carry is refused before its
	// bytes are on this disk.
	limit := int64(0)
	if exportID != "" {
		limit = remoteArtifactMaxBytes
	}
	digest := sha256.New()
	size := int64(0)
	write := func(text string) error {
		if limit > 0 && size+int64(len(text)) > limit {
			return threadExportTooLarge(size + int64(len(text)))
		}
		n, err := file.WriteString(text)
		size += int64(n)
		digest.Write([]byte(text[:n]))
		return err
	}
	exportErr := t.writeExport(ctx, q, write)
	closeErr := file.Close()
	if exportErr != nil {
		removeThreadExportTemp(temp)
		return threadtools.ExportFile{}, exportErr
	}
	if closeErr != nil {
		removeThreadExportTemp(temp)
		return threadtools.ExportFile{}, fmt.Errorf("thread tools: close export file: %w", closeErr)
	}
	if err := os.Rename(temp, path); err != nil {
		removeThreadExportTemp(temp)
		return threadtools.ExportFile{}, fmt.Errorf("thread tools: publish export file: %w", err)
	}
	if exportID != "" {
		return threadtools.ExportFile{ExportID: exportID, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
	}
	return threadtools.ExportFile{Path: path, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}

// removeThreadExportTemp drops the half-written render. Nothing else will:
// the sweep knows the answer and peer prefixes, not this one.
func removeThreadExportTemp(temp string) {
	if err := os.Remove(temp); err != nil && !os.IsNotExist(err) {
		log.Printf("thread tools: remove unfinished export %s: %v", temp, err)
	}
}

// threadExportChunkBytes is how much of a clipped item's remainder the
// export reads at a time. A file is written whole, but never through a
// whole item in memory.
//
// threadExportBatchBytes is the same promise for a batch of items. The
// store's LIMIT counts rows, so the row count is the byte budget divided
// by the most one row can carry, and whatever a row holds past that is
// streamed by writeExportBody.
const (
	threadExportChunkBytes = 256 << 10
	threadExportBatchBytes = 8 << 20
	threadExportBatchItems = threadExportBatchBytes / threadExportChunkBytes
)

// writeExportBody writes one included item's body whole.
//
// The transcript clips a large item and points at thread_item; a file does
// not (docs/specs/agent-thread-tools.md, thread_show: with to_file included
// items are written whole), so whatever the page clipped is streamed out of
// the store behind it, a chunk at a time.
func (t threadToolsApp) writeExportBody(
	ctx context.Context, threadID string, item threadtools.Item, write func(string) error,
) error {
	if !item.Clipped {
		return write(strings.TrimRight(item.Text, "\n"))
	}
	if err := write(item.Text); err != nil {
		return err
	}
	// The row is read once for the whole remainder: resolving it per chunk
	// would cost two lookups for every 256KB of a multi-megabyte item.
	row, err := t.itemRow(threadID, item.ID)
	if err != nil {
		return err
	}
	for offset := int64(len(item.Text)); offset < item.Size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, _, err := t.itemBodyRange(row, offset, threadExportChunkBytes)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return fmt.Errorf("thread tools: export item %s: body ended at %d of %d bytes",
				item.ID, offset, item.Size)
		}
		offset += int64(len(data))
		text := string(data)
		if offset >= item.Size {
			text = strings.TrimRight(text, "\n")
		}
		if err := write(text); err != nil {
			return err
		}
	}
	return nil
}

// writeExport walks the window in batches. The thread is resolved by the
// export that called it, and the window's bounds are fixed for the whole
// file, so a batch is one range query and nothing is re-resolved per pass.
func (t threadToolsApp) writeExport(ctx context.Context, q threadtools.ExportQuery, write func(string) error) error {
	if q.Bounds.Empty {
		return write("(no items)\n")
	}
	from := q.Bounds.From
	turn := ""
	for from <= q.Bounds.To {
		items, err := t.transcriptPage(ctx, threadtools.TranscriptQuery{
			ThreadID: q.ThreadID, From: from, To: q.Bounds.To, Limit: threadExportBatchItems,
			Include: q.Include, MaxItemBytes: threadExportChunkBytes,
		})
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		for _, item := range items {
			if item.TurnID != "" && item.TurnID != turn {
				turn = item.TurnID
				if err := write("--- turn " + turn + " ---\n"); err != nil {
					return err
				}
			}
			role := item.Role
			if role == "" {
				role = item.Kind
			}
			head := "[" + role + " " + item.ID + "]"
			if item.Name != "" {
				head += " " + item.Name
			}
			if item.Text == "" && item.Size > 0 {
				// A row the include list left out states its size here
				// exactly as it does in the transcript; only what was
				// included is written whole.
				if err := write(head + " (" + strconv.FormatInt(item.Size, 10) + " bytes not shown)\n"); err != nil {
					return err
				}
				from = item.Position + 1
				continue
			}
			if err := write(head + " "); err != nil {
				return err
			}
			if err := t.writeExportBody(ctx, q.ThreadID, item, write); err != nil {
				return err
			}
			if err := write("\n"); err != nil {
				return err
			}
			from = item.Position + 1
		}
	}
	return nil
}

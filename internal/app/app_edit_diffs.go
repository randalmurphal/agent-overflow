package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"sort"
	"strings"
	"sync"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// EditDiffEntry is one edit tool call in the review pane's Edits
// selector: an Edit/Write/apply_patch, or a command execution whose
// inline diff was captured. Metadata only: the diff itself opens on
// selection via OpenEditDiff.
type EditDiffEntry struct {
	ItemID     string   `json:"itemId"`
	PayloadID  string   `json:"payloadId"`
	TurnIndex  int      `json:"turnIndex"`
	Title      string   `json:"title"`
	Paths      []string `json:"paths"`
	Insertions int      `json:"insertions"`
	Deletions  int      `json:"deletions"`
	CreatedAt  int64    `json:"createdAt"`
}

// EditDiffTurnLabel captions a selector turn group with the turn's
// first user prompt.
type EditDiffTurnLabel struct {
	TurnIndex int    `json:"turnIndex"`
	Label     string `json:"label"`
}

type EditDiffList struct {
	Entries    []EditDiffEntry     `json:"entries"`
	TurnLabels []EditDiffTurnLabel `json:"turnLabels"`
}

// Selector labels render inside a NATIVE <select>/<optgroup> popup,
// which sizes itself to its longest label with no CSS truncation
// available — an uncapped multi-line prompt as a label stretches the
// popup across every monitor. Collapse whitespace to single spaces
// and cap well under a screen width.
const maxEditSelectorLabelRunes = 80

func editSelectorLabel(s string) string {
	return textgen.CapRunesWithEllipsis(strings.Join(strings.Fields(s), " "), maxEditSelectorLabelRunes)
}

// ListThreadEditDiffs lists a thread's edit tool calls for the review
// pane's Edits scope, grouped client-side by turn via TurnLabels.
//
//ao:scope threads:read
func (a *App) ListThreadEditDiffs(threadID string) (EditDiffList, error) {
	const action = "list thread edit diffs"
	if _, err := a.store.GetThread(threadID); err != nil {
		return EditDiffList{}, fmt.Errorf("%s: %w", action, err)
	}
	// A streaming turn's payload writes may still sit in triage buffers;
	// flush so an in-progress turn's edits are listable immediately.
	if err := a.readyThreadHistory(threadID); err != nil {
		return EditDiffList{}, fmt.Errorf("%s: %w", action, err)
	}
	rows, err := a.store.ListEditDiffItems(threadID)
	if err != nil {
		return EditDiffList{}, fmt.Errorf("%s: %w", action, err)
	}

	entries := make([]EditDiffEntry, 0, len(rows))
	turnsWithEdits := make(map[int]bool, 8)
	for _, row := range rows {
		entry := EditDiffEntry{
			ItemID:    row.ItemID,
			PayloadID: row.PayloadID,
			TurnIndex: row.TurnIndex,
			CreatedAt: row.CreatedAt,
			Title:     "File change",
			Paths:     []string{},
		}
		switch row.PayloadKind {
		case "diff":
			// Legacy Claude EventDiff attach: the meta is a DiffMeta, not
			// a ToolResultMeta.
			var meta triage.DiffMeta
			if json.Unmarshal([]byte(row.PayloadMeta), &meta) == nil && meta.FilePath != "" {
				entry.Title = "Edited " + meta.FilePath
				entry.Paths = append(entry.Paths, meta.FilePath)
				entry.Insertions = meta.Insertions
				entry.Deletions = meta.Deletions
			}
		default:
			var meta triage.ToolResultMeta
			if json.Unmarshal([]byte(row.PayloadMeta), &meta) == nil {
				if meta.Title != "" {
					entry.Title = meta.Title
				}
				if meta.InlineDiff != nil {
					entry.Insertions = meta.InlineDiff.Insertions
					entry.Deletions = meta.InlineDiff.Deletions
					for _, file := range meta.InlineDiff.Files {
						if file.Path != "" {
							entry.Paths = append(entry.Paths, file.Path)
						}
					}
				}
			}
		}
		entry.Title = editSelectorLabel(entry.Title)
		entries = append(entries, entry)
		turnsWithEdits[row.TurnIndex] = true
	}

	labels := []EditDiffTurnLabel{}
	if len(entries) > 0 {
		summaries, err := a.store.ListTurnUserSummaries(threadID)
		if err != nil {
			return EditDiffList{}, fmt.Errorf("%s: %w", action, err)
		}
		for _, summary := range summaries {
			if turnsWithEdits[summary.TurnIndex] {
				labels = append(labels, EditDiffTurnLabel{TurnIndex: summary.TurnIndex, Label: editSelectorLabel(summary.Summary)})
			}
		}
	}
	return EditDiffList{Entries: entries, TurnLabels: labels}, nil
}

// OpenEditDiff opens one edit tool call's diff for the review pane's Edits
// scope. It reads like every review diff: the first chunk here, the rest
// with ReadReviewDiff.
//
//ao:scope threads:read
func (a *App) OpenEditDiff(ctx context.Context, threadID string, payloadID string) (ReviewDiffOpened, error) {
	const action = "open edit diff"
	if _, err := a.store.GetThread(threadID); err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	if err := a.readyThreadHistory(threadID); err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	if _, err := a.getThreadPayloadMeta(threadID, payloadID); err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	return a.openEditsDiff(ctx, action, threadID, []string{payloadID})
}

// OpenTurnEditsDiff opens one turn's edit diffs joined in item order, the
// sequential story of what the turn changed. Nothing is merged: a file
// edited twice appears as two patch sections, each with the line numbers
// of its own moment. A turn with no edits is an empty diff.
//
//ao:scope threads:read
func (a *App) OpenTurnEditsDiff(ctx context.Context, threadID string, turnIndex int) (ReviewDiffOpened, error) {
	const action = "open turn edits diff"
	if _, err := a.store.GetThread(threadID); err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	if err := a.readyThreadHistory(threadID); err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	payloadIDs, err := a.store.ListTurnEditDiffPayloads(threadID, turnIndex)
	if err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	return a.openEditsDiff(ctx, action, threadID, payloadIDs)
}

func (a *App) openEditsDiff(ctx context.Context, action, threadID string, payloadIDs []string) (ReviewDiffOpened, error) {
	diff, err := newEditsDiff(ctx, a.store, threadID, payloadIDs)
	if err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	opened, err := a.holdOpenedReviewDiff(ctx, action, transport.ScopeThreadsRead, diff)
	if err != nil {
		return ReviewDiffOpened{}, err
	}
	opened.PayloadIDs = diff.payloadIDs()
	return opened, nil
}

// GetPayloadPatchSpans returns the highlight spans persisted for a diff
// payload, which the Edits scope seeds for the payloads an edits diff
// joins.
//
//ao:scope threads:read
func (a *App) GetPayloadPatchSpans(threadID string, payloadID string) ([]PatchSpanSeed, error) {
	if err := a.readyThreadHistory(threadID); err != nil {
		return nil, err
	}
	meta, err := a.getThreadPayloadMeta(threadID, payloadID)
	if err != nil {
		return nil, err
	}
	return a.persistedPayloadPatchSpans(threadID, meta.Kind, payloadID), nil
}

// errEditsDiffChanged is returned when an edit payload no longer holds the
// bytes an edits diff measured or served; the caller opens the diff again.
var errEditsDiffChanged = errors.New("edits diff: the diff changed since it was opened")

// editsDiffTailBytes is how much of a payload's end one read inspects for
// trailing newlines.
const editsDiffTailBytes = 256

// editsDiff joins edit payloads into one patch: each payload's bytes up to
// its trailing newlines, then one newline, so no blank line falls between
// two payloads' sections. Payload bytes are read from the store per chunk,
// so nothing holds the patch.
type editsDiff struct {
	store    *store.Store
	threadID string
	parts    []editsDiffPart
	size     int64

	mu   sync.Mutex
	seed maphash.Seed
	// served holds the hash of every chunk a read returned, keyed by its
	// byte range, so a re-read proves it serves the same bytes.
	served map[[2]int64]uint64
	// starts holds the offsets a read may start at: 0 and every offset a
	// chunk ended at.
	starts map[int64]bool
}

// editsDiffPart is one payload in an editsDiff. Its bytes [0, length)
// occupy [start, start+length) of the patch, followed by a newline. size
// is the payload's length when the diff was opened, which every read
// verifies.
type editsDiffPart struct {
	payloadID string
	start     int64
	length    int64
	size      int64
}

func newEditsDiff(ctx context.Context, st *store.Store, threadID string, payloadIDs []string) (*editsDiff, error) {
	d := &editsDiff{
		store:    st,
		threadID: threadID,
		seed:     maphash.MakeSeed(),
		served:   map[[2]int64]uint64{},
		starts:   map[int64]bool{0: true},
	}
	for _, payloadID := range payloadIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		size, length, err := d.measure(payloadID)
		if err != nil {
			return nil, err
		}
		if length == 0 {
			continue
		}
		d.parts = append(d.parts, editsDiffPart{payloadID: payloadID, start: d.size, length: length, size: size})
		d.size += length + 1
	}
	return d, nil
}

// measure returns a payload's length and how many of its bytes come before
// its trailing newlines.
func (d *editsDiff) measure(payloadID string) (size, length int64, err error) {
	_, total, _, err := d.store.GetPayloadChunk(d.threadID, payloadID, 0, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("read edit payload %s: %w", payloadID, err)
	}
	end := total
	for end > 0 {
		from := max(0, end-editsDiffTailBytes)
		tail, now, _, err := d.store.GetPayloadChunk(d.threadID, payloadID, from, end-from)
		if err != nil {
			return 0, 0, fmt.Errorf("read edit payload %s: %w", payloadID, err)
		}
		if now != total || len(tail) != end-from {
			return 0, 0, errEditsDiffChanged
		}
		kept := len(bytes.TrimRight(tail, "\n"))
		end = from + kept
		if kept > 0 {
			break
		}
	}
	return int64(total), int64(end), nil
}

func (d *editsDiff) payloadIDs() []string {
	ids := make([]string, len(d.parts))
	for i, part := range d.parts {
		ids[i] = part.payloadID
	}
	return ids
}

// Read returns up to maxBytes of the patch from offset under
// gitdiff.Diff.Read's contract.
func (d *editsDiff) Read(ctx context.Context, offset int64, maxBytes int) (gitdiff.Chunk, error) {
	maxBytes = min(max(maxBytes, gitdiff.MinChunkBytes), gitdiff.MaxChunkBytes)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.starts[offset] {
		return gitdiff.Chunk{}, fmt.Errorf("edits diff: offset %d is not a chunk boundary", offset)
	}
	if offset == d.size {
		return gitdiff.Chunk{Offset: offset, NextOffset: offset, EOF: true}, nil
	}
	// One byte past the window tells a full window at the end of the
	// patch apart from one with more to come.
	window, err := d.bytes(ctx, offset, min(offset+int64(maxBytes)+1, d.size))
	if err != nil {
		return gitdiff.Chunk{}, err
	}
	eof := offset+int64(len(window)) == d.size && len(window) <= maxBytes
	if !eof {
		window = window[:gitdiff.ChunkCut(window[:maxBytes])]
	}
	next := offset + int64(len(window))
	span := [2]int64{offset, next}
	sum := maphash.Bytes(d.seed, window)
	if prev, ok := d.served[span]; ok && prev != sum {
		return gitdiff.Chunk{}, errEditsDiffChanged
	}
	d.served[span] = sum
	d.starts[next] = true
	return gitdiff.Chunk{Data: string(window), Offset: offset, NextOffset: next, EOF: eof}, nil
}

// bytes returns the patch's bytes [from, to).
func (d *editsDiff) bytes(ctx context.Context, from, to int64) ([]byte, error) {
	buf := make([]byte, 0, to-from)
	// The part whose bytes or closing newline hold from.
	i := sort.Search(len(d.parts), func(i int) bool { return d.parts[i].start+d.parts[i].length >= from })
	for pos := from; pos < to; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part := d.parts[i]
		if end := part.start + part.length; pos < end {
			n := min(to, end) - pos
			data, size, _, err := d.store.GetPayloadChunk(d.threadID, part.payloadID, int(pos-part.start), int(n))
			if err != nil {
				return nil, fmt.Errorf("read edit payload %s: %w", part.payloadID, err)
			}
			if int64(size) != part.size || int64(len(data)) != n {
				return nil, errEditsDiffChanged
			}
			buf = append(buf, data...)
			pos += n
		}
		if pos < to {
			buf = append(buf, '\n')
			pos++
		}
	}
	return buf, nil
}

// Close releases nothing: an edits diff holds only its payload list.
func (d *editsDiff) Close() error { return nil }

package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/textgen"
	"agent-overflow/internal/triage"
)

// EditDiffEntry is one edit tool call in the review pane's Edits
// selector: an Edit/Write/apply_patch, or a command execution whose
// inline diff was captured. Metadata only — the diff itself loads on
// selection via GetPayloadData.
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

// A whole turn's concatenated edit diffs stay bounded like every other
// review-pane diff source (gitdiff.maxDiffOutputBytes is the sibling).
const maxTurnEditsDiffBytes = 10 * 1024 * 1024

// ListThreadEditDiffs lists a thread's edit tool calls for the review
// pane's Edits scope, grouped client-side by turn via TurnLabels.
func (a *App) ListThreadEditDiffs(threadID string) (EditDiffList, error) {
	const action = "list thread edit diffs"
	if _, err := a.store.GetThread(threadID); err != nil {
		return EditDiffList{}, fmt.Errorf("%s: %w", action, err)
	}
	// A streaming turn's payload writes may still sit in triage buffers;
	// flush so an in-progress turn's edits are listable immediately.
	if err := a.flushThreadPayloadBuffers(threadID); err != nil {
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

// TurnEditsDiff is a whole turn's concatenated edit diffs plus the
// constituent payloads' persisted highlight spans. Per-file content
// addressing makes the span union safe: a file edited once in the turn
// keys identically to its payload's section and paints primed; a file
// edited twice renders merged (different text, different key) and
// falls back to the RPC path.
type TurnEditsDiff struct {
	Data       string          `json:"data"`
	PatchSpans []PatchSpanSeed `json:"patchSpans,omitempty"`
}

// GetTurnEditsDiff returns one turn's edit diffs concatenated in item
// order — the sequential story of what the turn changed. Nothing is
// merged: a file edited twice appears as two patch sections, each with
// the line numbers of its own moment.
func (a *App) GetTurnEditsDiff(threadID string, turnIndex int) (TurnEditsDiff, error) {
	const action = "get turn edits diff"
	if _, err := a.store.GetThread(threadID); err != nil {
		return TurnEditsDiff{}, fmt.Errorf("%s: %w", action, err)
	}
	if err := a.flushThreadPayloadBuffers(threadID); err != nil {
		return TurnEditsDiff{}, fmt.Errorf("%s: %w", action, err)
	}
	patches, err := a.store.ListTurnEditDiffPatches(threadID, turnIndex)
	if err != nil {
		return TurnEditsDiff{}, fmt.Errorf("%s: %w", action, err)
	}
	var combined strings.Builder
	var spans []PatchSpanSeed
	for _, patch := range patches {
		text := strings.TrimRight(string(patch.Data), "\n")
		if text == "" {
			continue
		}
		if combined.Len() > 0 {
			combined.WriteByte('\n')
		}
		combined.WriteString(text)
		combined.WriteByte('\n')
		if combined.Len() > maxTurnEditsDiffBytes {
			return TurnEditsDiff{}, fmt.Errorf("%s: turn %d exceeds %d bytes — open its edits individually", action, turnIndex, maxTurnEditsDiffBytes)
		}
		spans = append(spans, a.loadPersistedPatchSpans(threadID, patch.PayloadID)...)
	}
	return TurnEditsDiff{Data: combined.String(), PatchSpans: spans}, nil
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/harnessclient"
)

// The selector every `--thread` accepts, and the resolution behind it.
//
// A thread id is a uuid: not a thing anyone types twice correctly, and
// not a thing an agent can produce without a prior listing. So four
// spellings resolve to one row, all against the SAME listing `threads`
// prints, in the same order:
//
//   - the full id
//   - `#N`, the index column that listing prints
//   - `last`, the most recently updated row
//   - a case-insensitive TITLE PREFIX, when exactly one row matches
//
// Resolution happens BEFORE the command's own RPC, and a miss is an
// error. `items --thread garbage` used to print "no items" and exit 0,
// which reads as "that thread is empty" — the wrong finding entirely,
// and the one a caller is least likely to double-check.

// threadRow is the subset of store.Thread this CLI prints and selects
// on. Declared locally so the binary does not link the store package
// (and its SQLite driver) to render five columns; -o json passes the
// server's own bytes through untouched, so nothing is lost by not
// typing the rest.
type threadRow struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	WorkspacePath string `json:"workspacePath"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// listThreadRows reads the harness escape hatch rather than
// App.ListThreads: a draft row with no items is invisible to the
// production read, and "was a row created" is exactly the question this
// CLI gets asked.
func listThreadRows(ctx context.Context, client *harnessclient.Client) ([]threadRow, json.RawMessage, error) {
	raw, err := client.Call(ctx, "HarnessListThreadRows")
	if err != nil {
		return nil, nil, err
	}
	var rows []threadRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, fmt.Errorf("decode thread rows: %w", err)
	}
	return rows, raw, nil
}

// resolveThreadSelector turns any accepted spelling into one row's id.
func resolveThreadSelector(ctx context.Context, client *harnessclient.Client, selector string) (threadRow, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return threadRow{}, usagef("--thread needs a value (an id, #N, `last`, or a title prefix)")
	}
	rows, _, err := listThreadRows(ctx, client)
	if err != nil {
		return threadRow{}, err
	}
	return pickThread(rows, selector)
}

// pickThread is resolveThreadSelector's pure half, so every spelling and
// every refusal is testable without a backend.
func pickThread(rows []threadRow, selector string) (threadRow, error) {
	if len(rows) == 0 {
		return threadRow{}, fmt.Errorf("no thread %q: this instance has no threads (seed one, or see `ao-harness threads`)", selector)
	}

	if index, ok := strings.CutPrefix(selector, "#"); ok {
		n, err := strconv.Atoi(index)
		if err != nil || n < 1 || n > len(rows) {
			return threadRow{}, usagef("no thread %q: the index column runs #1..#%d (see `ao-harness threads`)", selector, len(rows))
		}
		return rows[n-1], nil
	}

	if strings.EqualFold(selector, "last") {
		newest := rows[0]
		for _, row := range rows[1:] {
			if row.UpdatedAt > newest.UpdatedAt {
				newest = row
			}
		}
		return newest, nil
	}

	for _, row := range rows {
		if strings.EqualFold(row.ID, selector) {
			return row, nil
		}
	}

	var matches []threadRow
	lower := strings.ToLower(selector)
	for _, row := range rows {
		if strings.HasPrefix(strings.ToLower(row.Title), lower) {
			matches = append(matches, row)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return threadRow{}, fmt.Errorf("no thread %q (see `ao-harness threads`)", selector)
	default:
		return threadRow{}, usagef("%q matches %d threads; name one%s", selector, len(matches), threadCandidates(rows, matches))
	}
}

// threadCandidates lists the ambiguous matches WITH the index a caller
// can retype, which is the whole reason the listing grew one.
func threadCandidates(all, matches []threadRow) string {
	index := make(map[string]int, len(all))
	for i, row := range all {
		index[row.ID] = i + 1
	}
	var b strings.Builder
	b.WriteString(":")
	for _, row := range matches {
		fmt.Fprintf(&b, "\n  #%-3d %s  %s", index[row.ID], row.ID, truncate(row.Title, 60))
	}
	return b.String()
}

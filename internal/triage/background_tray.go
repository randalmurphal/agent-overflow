package triage

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// The background tray's deltas (provider:background_tray).
//
// A Claude thread's tray is the rows Store.ListLiveBackgroundTasks serves.
// A write that changes one of them announces the launches it changed, and
// the frame carries those launches' rows as Store.ListBackgroundTrayRows
// serves them, so a client applies the change without reading the list
// again. The cost of a frame is the named launches' rows, never the
// thread's other tasks. The writes that announce:
//
//   - every pushed row (appendTrayLaunches): a background launch; a
//     completion sibling, ending or parked, naming its launch; an agent's
//     wake, naming the transcript root it is filed under, whose carriers
//     the delta serves with it (Store.ListBackgroundTrayRows); and a
//     nested call settling that may be an agent launch, which leaves the
//     tray's class 2. The write's own probe of the row
//     (store.ProbeWireItem) says whether it may anchor, so a plain call
//     settling under an agent costs no read here. A queued row announces
//     when the drain persists it;
//   - a pushed anchor below the top level (wire_items.go): a nested
//     launch joins the tray at its first child and its latest-tool line
//     moves with its card;
//   - a §E6 rebind retiring a parked agent's rows
//     (retireParkedLaunchesForRebind), which no push carries.
//
// Each frame reads the rows after its write committed, under the thread's
// tray lock, so frames reach the bus in the order of the reads they carry:
// a client that applies them in sequence ends at the last read.
//
// A Codex thread's tray also lists runtime records no store row carries,
// so its frames ask for a refresh, as does a delta whose read failed.

// BackgroundTrayEvent is the provider:background_tray payload. A delta
// names the launches it answers for in LaunchIDs and carries their rows
// in Rows; a named launch with no row has left the tray. Refresh asks the
// client to read the whole list instead.
type BackgroundTrayEvent struct {
	ThreadID  string       `json:"threadId"`
	LaunchIDs []string     `json:"launchIds,omitempty"`
	Rows      []store.Item `json:"rows,omitempty"`
	Refresh   bool         `json:"refresh,omitempty"`
}

// emitBackgroundTray announces the tray rows of the named launches.
func (r *Router) emitBackgroundTray(threadID string, launchIDs ...string) {
	threadID = strings.TrimSpace(threadID)
	ids := trayLaunchIDs(launchIDs)
	if threadID == "" || len(ids) == 0 {
		return
	}
	codex, err := r.isCodexThread(threadID)
	if err != nil {
		log.Printf("triage: background tray delta for %s: %v", threadID, err)
		r.emitBackgroundTrayRefresh(threadID)
		return
	}
	if codex {
		r.emitBackgroundTrayRefresh(threadID)
		return
	}
	r.emitBackgroundTrayRows(threadID, ids)
}

// emitBackgroundTrayRows reads and pushes the rows of ids, a Claude
// thread's launches.
func (r *Router) emitBackgroundTrayRows(threadID string, ids []string) {
	lock := &r.identity(threadID).trayLock
	lock.Lock()
	defer lock.Unlock()
	cutoff := time.Now().UnixMilli() - store.BackgroundTaskRetentionMillis
	rows, err := r.store.ListBackgroundTrayRows(threadID, cutoff, ids)
	if err != nil {
		log.Printf("triage: read the background tray rows of %v on %s: %v", ids, threadID, err)
		r.emit(eventchan.ProviderBackgroundTray, BackgroundTrayEvent{ThreadID: threadID, Refresh: true})
		return
	}
	r.emit(eventchan.ProviderBackgroundTray, BackgroundTrayEvent{
		ThreadID:  threadID,
		LaunchIDs: ids,
		Rows:      itemwire.ProjectItems(rows, true),
	})
}

// emitBackgroundTrayRefresh asks the thread's tray to read its whole list:
// the change is in a source a delta does not carry.
func (r *Router) emitBackgroundTrayRefresh(threadID string) {
	if strings.TrimSpace(threadID) == "" {
		return
	}
	r.emit(eventchan.ProviderBackgroundTray, BackgroundTrayEvent{ThreadID: threadID, Refresh: true})
}

// appendTrayLaunches appends the tray launches a pushed row of threadID
// changed. anchors is the write's probe of the row (store.WireItemProbe):
// a settled call below the top level leaves the tray only if it is an
// agent launch, and one that anchors nothing is not.
func appendTrayLaunches(ids []string, threadID string, row store.Item, anchors bool) []string {
	if row.ThreadID != threadID {
		return ids
	}
	parent := strings.TrimSpace(row.ParentID)
	switch {
	case row.CompletionOf != "":
		return append(ids, row.CompletionOf)
	case isWakePromptRow(row):
		return append(ids, parent)
	case row.Kind != itemKindToolCall:
	case row.IsBackground:
		return append(ids, row.ID)
	case parent != "" && row.Status != statusRunning && anchors:
		return append(ids, row.ID)
	}
	return ids
}

// announceTrayLaunches announces the tray launches a batch of pushed rows
// changed, in one frame. A Codex thread's tray takes these rows from its
// own pushes and runtime refreshes (activityRailBackground.svelte.ts), so
// the batch announces nothing there.
func (r *Router) announceTrayLaunches(threadID string, launchIDs []string) {
	ids := trayLaunchIDs(launchIDs)
	if len(ids) == 0 {
		return
	}
	codex, err := r.isCodexThread(threadID)
	if err != nil {
		log.Printf("triage: background tray rows for %s: %v", threadID, err)
		r.emitBackgroundTrayRefresh(threadID)
		return
	}
	if !codex {
		r.emitBackgroundTrayRows(threadID, ids)
	}
}

// isWakePromptRow reports whether a pushed row is an agent's wake
// (writeWakePromptRow): the row the served run state reads to run the
// agent at its root again (Store.CurrentParkedStop). A meta the store
// cannot read is not a wake there either.
func isWakePromptRow(row store.Item) bool {
	if row.Kind != itemKindUserText || strings.TrimSpace(row.ParentID) == "" ||
		!strings.Contains(row.Meta, provider.MetaSubagentWakePromptKey) {
		return false
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		log.Printf("triage: decode wake candidate %s/%s meta: %v", row.ThreadID, row.ID, err)
		return false
	}
	return string(meta[provider.MetaSubagentWakePromptKey]) == "true"
}

// isNestedTrayCandidate reports whether a pushed anchor below the top
// level can be a tray row whose push the tray does not receive: a running
// call, which joins the tray at its first child (an agent under a
// background launch), or a live background launch whose latest-tool line
// moved with its card. A background launch keeps status running after it
// settles; its live_background_active flag is what says it left.
func isNestedTrayCandidate(it store.Item) bool {
	if it.Kind != itemKindToolCall || it.Status != statusRunning || strings.TrimSpace(it.ParentID) == "" {
		return false
	}
	if !it.IsBackground || strings.TrimSpace(it.Meta) == "" {
		return true
	}
	var meta struct {
		Active *bool `json:"live_background_active"`
	}
	if err := json.Unmarshal([]byte(it.Meta), &meta); err != nil {
		// The delta decides: a launch that is not in the tray reads none.
		log.Printf("triage: decode tray candidate %s/%s meta: %v", it.ThreadID, it.ID, err)
		return true
	}
	return meta.Active == nil || *meta.Active
}

// trayLaunchIDs trims, drops empty and dedupes launch ids, keeping order.
func trayLaunchIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if _, dup := seen[id]; id == "" || dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

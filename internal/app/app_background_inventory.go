package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// The cross-thread view of what a host is currently running.
//
// Every background-task surface before this one is thread-scoped: the
// tray reads one thread, the reaper probes one thread, the stop RPCs
// take one thread. That is the right shape for someone looking at a
// pane, and the wrong shape for someone asking "what is this machine
// doing right now" — which is the only question available when the
// machine is somewhere else.

// Background-work kinds. The vocabulary is closed and each value names
// exactly one existing stop RPC, so a client that renders a row already
// knows which call its Stop button makes. Nothing here is a new way to
// terminate anything; the ids are handles into the stop paths that
// already exist.
const (
	// BackgroundWorkClaudeTask — StopClaudeTask(threadId, stopId),
	// where stopId is the Claude `task_id`. Empty on claude-tui
	// launches: that provider never reconstructs `system/task_started`,
	// so the row is real but has no per-task handle.
	BackgroundWorkClaudeTask = "claudeTask"
	// BackgroundWorkCodexSubagent — StopCodexSubagent(threadId, stopId),
	// where stopId is the launch row's own id; the provider resolves it
	// to the child threads it owns.
	BackgroundWorkCodexSubagent = "codexSubagent"
	// BackgroundWorkCodexTerminal — TerminateCodexBackgroundTerminal(
	// threadId, stopId), where stopId is the PTY `process_id`. Empty
	// until the wire names a process.
	BackgroundWorkCodexTerminal = "codexTerminal"
)

// RunningBackgroundWork is one live background task, attributed to the
// thread that owns it.
//
// The payload is deliberately narrower than the tray's `store.Item`: no
// meta blob, no payload ids, no completion siblings. An inventory
// answers which thread, what, and since when — the tray's richer row is
// what a client asks for once it has navigated somewhere.
type RunningBackgroundWork struct {
	ThreadID    string `json:"threadId"`
	ThreadTitle string `json:"threadTitle"`
	ProjectID   string `json:"projectId,omitempty"`
	Provider    string `json:"provider"`
	// Kind selects the stop RPC; StopID is the handle that RPC takes.
	// An empty StopID means this task has no per-task handle — stopping
	// it goes through StopThreadBackgroundWork or the session itself.
	Kind   string `json:"kind"`
	StopID string `json:"stopId,omitempty"`
	// ItemID is the timeline row, so a client can navigate to the task
	// rather than only stop it. ParentItemID carries the nesting the
	// tray indents by.
	ItemID       string `json:"itemId"`
	ParentItemID string `json:"parentItemId,omitempty"`
	ToolName     string `json:"toolName,omitempty"`
	Summary      string `json:"summary,omitempty"`
	// StartedAt is the launch row's created_at, in Unix milliseconds.
	StartedAt int64 `json:"startedAt"`
}

// BackgroundWorkInventory is ListRunningBackgroundWork's answer: the
// running rows, oldest first, plus the ids of any live-session threads
// whose rows could not be read.
//
// Incompleteness rides the payload rather than the error return because
// the wire delivers one or the other, never both: a bound method's
// non-nil error discards its result at the dispatcher, so "partial rows
// plus an error" would reach a client as no rows at all. A caller
// deciding what to shut down wants the rows it CAN see and the fact
// that a thread went unread; the full error text is logged server-side.
type BackgroundWorkInventory struct {
	Rows []RunningBackgroundWork `json:"rows"`
	// UnreadableThreadIDs names live-session threads whose task rows
	// failed to read. Empty on the ordinary path; non-empty means the
	// inventory is a lower bound, not the whole answer.
	UnreadableThreadIDs []string `json:"unreadableThreadIds,omitempty"`
}

// ListRunningBackgroundWork returns every background task running right
// now, across every thread, oldest first — the answer to "what is this
// host still carrying" from a client that cannot look at the machine.
//
// Scope is the set of threads with a LIVE provider session, and that is
// the honest domain rather than a shortcut. All three sources of
// background work are session-bound: a Claude task dies with its
// process group, a Codex background terminal and a spawned child belong
// to that thread's app-server, and the transient unified-exec trackers
// live in the triage router and are dropped when the session ends. A
// `running` row on a thread with no session is a residue the boot sweep
// settles, not work in progress; listing it would invite a client to
// stop something that is not there.
//
// Per thread it calls ListLiveBackgroundTasks — the SAME composition
// the tray reads, which is the point. That method already unions the
// three sources (the store query, live Codex subagent launches, and the
// triage layer's in-memory Codex unified-exec tasks, which exist in no
// table); re-deriving the union here would give a SQLite-only answer
// that silently under-reports the third source. Only the projection
// differs: the tray's completed-sibling retention window is a
// live-render tuning value, so the terminal rows it carries are dropped
// and an inventory reports what is running.
//
//ao:scope threads:read
func (a *App) ListRunningBackgroundWork() (BackgroundWorkInventory, error) {
	live := a.sessionManager().snapshot()
	threadIDs := make([]string, 0, len(live))
	for threadID := range live {
		threadIDs = append(threadIDs, threadID)
	}
	sort.Strings(threadIDs)

	var inv BackgroundWorkInventory
	for _, threadID := range threadIDs {
		rows, err := a.runningBackgroundWorkForThread(threadID, live[threadID].Provider)
		if err != nil {
			// One unreadable thread must not blank the whole
			// inventory: a partial answer that names what it could
			// not read beats no answer at all when the caller is
			// deciding what to shut down. The wire cannot carry rows
			// beside an error (see BackgroundWorkInventory), so the
			// gap is reported in the payload and detailed in the log.
			log.Printf("app: list running background work: thread %s: %v", threadID, err)
			inv.UnreadableThreadIDs = append(inv.UnreadableThreadIDs, threadID)
			continue
		}
		inv.Rows = append(inv.Rows, rows...)
	}

	sort.SliceStable(inv.Rows, func(i, j int) bool {
		if inv.Rows[i].StartedAt != inv.Rows[j].StartedAt {
			return inv.Rows[i].StartedAt < inv.Rows[j].StartedAt
		}
		if inv.Rows[i].ThreadID != inv.Rows[j].ThreadID {
			return inv.Rows[i].ThreadID < inv.Rows[j].ThreadID
		}
		return inv.Rows[i].ItemID < inv.Rows[j].ItemID
	})
	inv.Rows = slicesx.OrEmpty(inv.Rows)
	return inv, nil
}

// StopThreadBackgroundWork terminates every background task one thread
// is running and returns how many it stopped. The provider session
// stays up: this is the control for "stop what the agent left running",
// not for "release the thread" — archiving does the latter.
//
// Each task goes through the same per-task RPC a client would call for
// that row (StopClaudeTask, StopCodexSubagent,
// TerminateCodexBackgroundTerminal), so there is exactly one
// termination path per provider and this method adds none. Failures are
// joined rather than short-circuiting: a task that refuses to die must
// not keep the rest alive.
//
//ao:scope threads:operate
func (a *App) StopThreadBackgroundWork(threadID string) (int, error) {
	if a.shuttingDown.Load() {
		return 0, ErrShuttingDown
	}
	providerName := ""
	if sess, live := a.sessionManager().get(threadID); live {
		providerName = sess.Provider
	}
	rows, err := a.runningBackgroundWorkForThread(threadID, providerName)
	if err != nil {
		return 0, err
	}

	stopped := 0
	var errs []error
	for _, row := range rows {
		ok, err := a.stopBackgroundWorkItem(row)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			stopped++
		}
	}
	if len(errs) > 0 {
		return stopped, fmt.Errorf("stop background work on thread %s: %w", threadID, errors.Join(errs...))
	}
	return stopped, nil
}

// stopBackgroundWorkItem routes one inventory row to the stop RPC its
// kind names. Reports whether a task was actually terminated — a row
// with no handle, or a handle the provider no longer recognizes,
// stopped nothing and must not be counted as if it had.
func (a *App) stopBackgroundWorkItem(row RunningBackgroundWork) (bool, error) {
	if row.StopID == "" {
		return false, nil
	}
	switch row.Kind {
	case BackgroundWorkClaudeTask:
		if err := a.StopClaudeTask(row.ThreadID, row.StopID); err != nil {
			return false, err
		}
		return true, nil
	case BackgroundWorkCodexSubagent:
		return a.StopCodexSubagent(row.ThreadID, row.StopID)
	case BackgroundWorkCodexTerminal:
		return a.TerminateCodexBackgroundTerminal(row.ThreadID, row.StopID)
	default:
		return false, fmt.Errorf("unknown background work kind %q on thread %s", row.Kind, row.ThreadID)
	}
}

// runningBackgroundWorkForThread projects one thread's live tray set
// down to the running rows and attributes each to its thread.
//
// providerName comes from the live session entry when there is one,
// because that is the process actually running the work; the thread row
// is the fallback for a caller that has no session in hand.
func (a *App) runningBackgroundWorkForThread(threadID, providerName string) ([]RunningBackgroundWork, error) {
	items, err := a.ListLiveBackgroundTasks(threadID)
	if err != nil {
		return nil, err
	}
	// The tray's set is launches plus their recently-completed siblings;
	// an inventory wants launches that are still running. Both halves of
	// the retention window have to go, and they look different: the
	// sibling row is terminal and drops on status, while the launch it
	// settles is still marked `running` (a launch row never flips) and
	// drops on the presence of that sibling. Every sibling in this set
	// is by definition a recent one — an older one would already have
	// excluded its launch from the query — so this does not
	// re-implement the retention rule, it undoes it.
	settled := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.CompletionOf != "" {
			settled[item.CompletionOf] = struct{}{}
		}
	}
	running := items[:0:0]
	for _, item := range items {
		if item.Status != statusRunningBackgroundWork {
			continue
		}
		if _, done := settled[item.ID]; done {
			continue
		}
		running = append(running, item)
	}
	if len(running) == 0 {
		return nil, nil
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, fmt.Errorf("thread %s: %w", threadID, err)
	}
	if providerName == "" {
		providerName = thread.Provider
	}

	rows := make([]RunningBackgroundWork, 0, len(running))
	seen := make(map[string]struct{}, len(running))
	for _, item := range running {
		// The three sources are disjoint by construction (the store
		// leg wants status='running', the subagent leg status
		// 'completed', the triage leg is not persisted at all), but an
		// inventory that double-counted would misreport the host's
		// load, so the union is made explicit rather than assumed.
		if _, dup := seen[item.ID]; dup {
			continue
		}
		seen[item.ID] = struct{}{}
		kind, stopID := backgroundWorkHandle(providerName, item)
		rows = append(rows, RunningBackgroundWork{
			ThreadID:     threadID,
			ThreadTitle:  thread.Title,
			ProjectID:    thread.ProjectID,
			Provider:     providerName,
			Kind:         kind,
			StopID:       stopID,
			ItemID:       item.ID,
			ParentItemID: item.ParentID,
			ToolName:     item.ToolName,
			Summary:      item.Summary,
			StartedAt:    item.CreatedAt,
		})
	}
	return rows, nil
}

// statusRunningBackgroundWork is the item status an unfinished
// background launch carries. Spelled once here so the inventory's
// running-only filter cannot drift from the tray query that produces
// the rows.
const statusRunningBackgroundWork = "running"

// backgroundWorkHandle resolves which stop RPC owns a row and the id it
// takes. Keyed on the provider first because the two id namespaces are
// not interchangeable — Claude stops by task id, Codex by process id or
// launch id — which is the same branch every existing caller of those
// RPCs has to make.
func backgroundWorkHandle(providerName string, item store.Item) (kind, stopID string) {
	switch providerName {
	case string(provider.Codex):
		if item.ToolName == codexSubagentToolName {
			return BackgroundWorkCodexSubagent, item.ID
		}
		return BackgroundWorkCodexTerminal, itemMetaString(item.Meta, "process_id")
	default:
		return BackgroundWorkClaudeTask, itemMetaString(item.Meta, "task_id")
	}
}

// codexSubagentToolName is the tool_name a Codex spawned-child launch
// row carries; the store's live-subagent query selects on the same
// value.
const codexSubagentToolName = "collab_agent"

// itemMetaString reads one top-level string field out of an item's meta
// JSON. A malformed or absent field yields "", which callers render as
// "this row has no stop handle" rather than as an error: meta is
// provider-shaped and a missing key is a normal state, not a fault.
func itemMetaString(raw, key string) string {
	if raw == "" {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return ""
	}
	value, ok := fields[key]
	if !ok {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return ""
	}
	return text
}

package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// The other threads a thread is waiting on, as its activity rail's Agents
// segment lists them (docs/specs/standing-agents.md, "Agents segment"). The
// rows are the thread's own spawn, send and ask requests (`thread_requests`)
// with each target's live state beside them; a spawned thread's own requests
// nest under its row. Nothing here keeps a copy of a request: every row is
// the ledger's, rendered through requestStateView, the thread tools' own
// derivation, which records no delivery.
//
// Every write to a request (OnThreadRequestChanged), every wait parked on or
// leaving one, and every change to a running target the row shows (turn,
// prompt, title, latest activity) emits `thread:agent_rows_changed` for the
// request's caller. A pane reads its list again on a frame that names its
// thread or a thread its list nests.

const (
	// threadAgentRowLimit bounds one listing, nested rows included. A
	// thread past it is told how many it was not shown.
	threadAgentRowLimit = 128
	// threadAgentRowDepth bounds the nesting: a listed thread's own rows
	// are depth 0, and rows at depth threadAgentRowDepth-1 nest nothing
	// further. A row at the bound whose thread has agents of its own says
	// so (NestedHidden).
	threadAgentRowDepth = 4
	// threadAgentSettledWindow is how long a settled request stays in the
	// listing. The pane holds a settled row for its own linger (8 s, from
	// when it first saw the row settled); the window is longer so a read
	// that arrives late, after a reconnect or a coalesced burst, still
	// carries the outcome.
	threadAgentSettledWindow = 30 * time.Second
	// threadAgentLineMax bounds a row's activity, blocked-on and answer
	// lines, in runes.
	threadAgentLineMax = 240
	// threadAgentActivityEvery bounds how often a target's latest activity
	// alone emits a frame.
	threadAgentActivityEvery = 2 * time.Second
	// threadAgentStopsInFlight bounds a Stop All's concurrent stops: each
	// may wait out a paired computer's cancel.
	threadAgentStopsInFlight = 8
)

// ThreadAgentRow is one request a thread made of another thread, with that
// thread as it stands.
type ThreadAgentRow struct {
	Token string `json:"token"`
	// CallerThreadID made the request: the listed thread for a depth-0 row,
	// the thread a nested row's parent spawned otherwise.
	CallerThreadID string `json:"callerThreadId"`
	// Kind is spawn, send or ask.
	Kind string `json:"kind"`
	// ThreadID is the target; for an ask, the hidden fork that answers it.
	ThreadID string `json:"threadId,omitempty"`
	// OpenThreadID is the thread a click opens: the target, or for an ask
	// the thread it asked about.
	OpenThreadID string `json:"openThreadId,omitempty"`
	// ComputerID and Computer name a target on a paired computer; both are
	// empty for this one.
	ComputerID string `json:"computerId,omitempty"`
	Computer   string `json:"computer,omitempty"`
	Title      string `json:"title"`
	// Provider is a local target's provider; a paired computer's thread
	// carries none.
	Provider string `json:"provider,omitempty"`
	// State is the request state thread_status reports: running, blocked,
	// replied, finished, errored, cancelled, interrupted, expired, refused,
	// and the accepted and unconfirmed states before the work starts.
	State string `json:"state"`
	// BlockedOn is approval or question for a local target waiting on a
	// prompt, person for a paired computer's target that reports it is
	// blocked; BlockedDetail says what it asks and BlockedRoutedTo names
	// the standing agent its prompt went to (ruling 19).
	BlockedOn       string `json:"blockedOn,omitempty"`
	BlockedDetail   string `json:"blockedDetail,omitempty"`
	BlockedRoutedTo string `json:"blockedRoutedTo,omitempty"`
	// Activity is what a running local target did last: its latest
	// top-level tool call or message, while the request runs.
	Activity string `json:"activity,omitempty"`
	// LastMessage is who spoke last in a local target and the end of it.
	LastMessage     string `json:"lastMessage,omitempty"`
	LastMessageRole string `json:"lastMessageRole,omitempty"`
	// Answer is the head of a settled request's answer, AnswerKind its kind.
	Answer     string `json:"answer,omitempty"`
	AnswerKind string `json:"answerKind,omitempty"`
	// LateReply is a reply that arrived after the request settled.
	LateReply bool  `json:"lateReply,omitempty"`
	CreatedAt int64 `json:"createdAt"`
	SettledAt int64 `json:"settledAt,omitempty"`
	// WakeItemID is the caller's timeline row that delivered the answer,
	// once the provider confirmed it.
	WakeItemID string `json:"wakeItemId,omitempty"`
	Depth      int    `json:"depth"`
	// NestedHidden marks a row at the depth bound whose thread has agents
	// of its own that the listing does not show.
	NestedHidden bool `json:"nestedHidden,omitempty"`
	// NestedUnavailable says why a spawn on a paired computer shows none of
	// that thread's own agents: the computer did not answer, or runs a
	// version that cannot list them.
	NestedUnavailable string `json:"nestedUnavailable,omitempty"`
	// OwnerComputerID is the paired computer whose ledger holds a nested
	// row's request; empty for this computer's own rows.
	OwnerComputerID string `json:"ownerComputerId,omitempty"`
}

// ThreadAgentRows is a thread's agents in tree order: each row, then the
// rows its spawned thread made. Hidden counts rows the bound left out.
type ThreadAgentRows struct {
	Rows   []ThreadAgentRow `json:"rows"`
	Hidden bool             `json:"hidden,omitempty"`
}

// ThreadAgentRowChange is one `thread:agent_rows_changed` frame: a request
// of ThreadID changed. Row is the request as it stands; whether a settled
// one is still listed is the listing's call. Removed says it is no longer
// an agent (its wake was disarmed and nothing waits on it). A frame with
// neither is about every request of the thread.
type ThreadAgentRowChange struct {
	ThreadID string          `json:"threadId"`
	ID       string          `json:"id,omitempty"`
	Row      *ThreadAgentRow `json:"row,omitempty"`
	Removed  bool            `json:"removed,omitempty"`
}

// Stop outcomes of one Agents row.
const (
	threadAgentStopped = "stopped"
	threadAgentEnded   = "ended"
	threadAgentFailed  = "failed"
)

// ThreadAgentStop is what stopping one row did. Outcome is stopped, ended
// (the request had already settled) or failed (nothing was stopped). A
// spawn's Stop also reaches into the thread it made: Interrupted says its
// running turn was interrupted, Background counts the Backgrounded items
// stopped there, and Nested the Agents rows of its own stopped with it.
// Failures says what could not be stopped, one line per layer; a failure
// in one layer leaves the others to run.
type ThreadAgentStop struct {
	Token       string   `json:"token"`
	Outcome     string   `json:"outcome"`
	Interrupted bool     `json:"interrupted,omitempty"`
	Background  int      `json:"background,omitempty"`
	Nested      int      `json:"nested,omitempty"`
	Failures    []string `json:"failures,omitempty"`
	// Incoming marks the withdrawal of a message queued for the thread,
	// named by its send id in Token; ended then means it was delivered.
	Incoming bool `json:"incoming,omitempty"`
}

// ListThreadAgentRows lists the requests a thread made of other threads that
// it is waiting on, with their nested rows, those a paired computer holds
// included.
//
//ao:scope threads:read
func (a *App) ListThreadAgentRows(ctx context.Context, threadID string) (ThreadAgentRows, error) {
	if _, err := a.store.GetThread(threadID); err != nil {
		return ThreadAgentRows{}, fmt.Errorf("read the thread: %w", err)
	}
	listing, _, err := a.threadAgentTreeDepth(ctx, threadID, threadAgentRowDepth)
	return listing, err
}

// threadAgentEntry is one listed row and what stops it: the request here,
// or, for a row a paired computer listed, that computer, the thread here
// whose spawn its lineage starts from and the thread that spawn made there,
// under which that computer finds the row again.
type threadAgentEntry struct {
	request    *store.ThreadRequest
	peer       string
	peerSource string
	peerRoot   string
}

// threadAgentTree reads the listing and what stops each row.
func (a *App) threadAgentTree(ctx context.Context, threadID string) (ThreadAgentRows, []threadAgentEntry, error) {
	return a.threadAgentTreeDepth(ctx, threadID, threadAgentRowDepth)
}

// threadAgentTreeDepth is threadAgentTree nested at most maxDepth levels: a
// paired computer listing for a caller's nested row has fewer left.
func (a *App) threadAgentTreeDepth(ctx context.Context, threadID string, maxDepth int) (ThreadAgentRows, []threadAgentEntry, error) {
	view := newThreadAgentView(a)
	out := ThreadAgentRows{Rows: []ThreadAgentRow{}}
	var entries []threadAgentEntry
	visited := map[string]bool{threadID: true}
	var walk func(caller string, depth int) error
	walk = func(caller string, depth int) error {
		budget := threadAgentRowLimit - len(out.Rows)
		if budget <= 0 {
			out.Hidden = true
			return nil
		}
		rows, more, err := a.store.ListThreadAgentRequests(caller, view.waiting, view.since, budget)
		if err != nil {
			return err
		}
		out.Hidden = out.Hidden || more
		for _, row := range rows {
			if len(out.Rows) >= threadAgentRowLimit {
				out.Hidden = true
				return nil
			}
			index := len(out.Rows)
			out.Rows = append(out.Rows, view.row(row, depth))
			entries = append(entries, threadAgentEntry{request: &row})
			child := row.TargetThreadID
			if row.Kind != store.ThreadRequestSpawn || child == "" {
				continue
			}
			if row.TargetComputerID != "" {
				if depth+1 < maxDepth {
					a.nestPeerAgentRows(ctx, &out, &entries, index, row, depth+1, maxDepth)
				}
				continue
			}
			if visited[child] {
				continue
			}
			visited[child] = true
			if depth+1 >= maxDepth {
				nested, _, err := a.store.ListThreadAgentRequests(child, view.waiting, view.since, 1)
				if err != nil {
					return err
				}
				out.Rows[index].NestedHidden = len(nested) > 0
				continue
			}
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(threadID, 0); err != nil {
		return ThreadAgentRows{}, nil, err
	}
	view.attachLastMessages(out.Rows)
	return out, entries, nil
}

// nestPeerAgentRows nests the rows a paired computer lists for the thread a
// spawn made there under that spawn's row, named by that computer. A
// computer that cannot answer leaves the reason on the row.
func (a *App) nestPeerAgentRows(ctx context.Context, out *ThreadAgentRows, entries *[]threadAgentEntry, index int, spawn store.ThreadRequest, depth, maxDepth int) {
	listing, err := a.peerThreadAgentRows(ctx, spawn.TargetComputerID, spawn.CallerThreadID, spawn.TargetThreadID, maxDepth-depth)
	if err != nil {
		out.Rows[index].NestedUnavailable = err.Error()
		return
	}
	peerName := out.Rows[index].Computer
	self, _ := a.backendIdentity()
	for _, row := range listing.Rows {
		if len(out.Rows) >= threadAgentRowLimit {
			out.Hidden = true
			return
		}
		row.Depth += depth
		row.OwnerComputerID = spawn.TargetComputerID
		switch row.ComputerID {
		case "":
			// A thread on that computer.
			row.ComputerID, row.Computer = spawn.TargetComputerID, peerName
		case self:
			// A thread that computer reached back here on.
			row.ComputerID, row.Computer = "", ""
		}
		out.Rows = append(out.Rows, row)
		*entries = append(*entries, threadAgentEntry{
			peer: spawn.TargetComputerID, peerSource: spawn.CallerThreadID, peerRoot: spawn.TargetThreadID,
		})
	}
	out.Hidden = out.Hidden || listing.Hidden
}

// threadAgentView renders request rows for one read: the parked waits and
// the settled window are read once for the whole listing.
type threadAgentView struct {
	app     *App
	tools   threadToolsApp
	waiting []string
	since   int64
}

func newThreadAgentView(a *App) threadAgentView {
	return threadAgentView{
		app: a, tools: a.threadTools(), waiting: a.parkedRequestTokens(),
		since: time.Now().Add(-threadAgentSettledWindow).UnixMilli(),
	}
}

func (v threadAgentView) row(row store.ThreadRequest, depth int) ThreadAgentRow {
	state := v.tools.requestStateView(row)
	out := ThreadAgentRow{
		Token: row.Token, CallerThreadID: row.CallerThreadID, Kind: row.Kind,
		ThreadID: row.TargetThreadID, OpenThreadID: row.TargetThreadID,
		ComputerID: row.TargetComputerID, Title: state.Title, State: state.State,
		LateReply: len(row.LateReply) > 0, CreatedAt: row.CreatedAt, SettledAt: row.SettledAt, Depth: depth,
	}
	if row.Kind == store.ThreadRequestAsk && row.OriginThreadID != "" {
		// The fork that answers an ask is hidden; the thread it asked about
		// is what a person opens.
		out.OpenThreadID = row.OriginThreadID
	}
	if row.TargetComputerID != "" {
		out.Computer, _ = v.tools.threadToolsBackendName(row.TargetComputerID)
	}
	settled := threadRequestSettled(row)
	if settled {
		out.Answer = clipLine(state.Answer)
		out.AnswerKind = state.AnswerKind
		out.WakeItemID = v.app.threadAgentWakeItem(row)
	}
	if row.TargetComputerID == "" && row.TargetThreadID != "" {
		if thread, err := v.app.store.GetThread(row.TargetThreadID); err == nil {
			out.Provider = thread.Provider
		}
	}
	switch {
	case settled:
	case row.TargetComputerID != "":
		if state.State == threadtools.RequestBlocked {
			out.BlockedOn = "person"
		}
	case row.TargetThreadID != "":
		if v.app.triage != nil {
			snapshot := v.app.triage.LiveStateSnapshotForThread(row.TargetThreadID)
			out.BlockedOn, out.BlockedDetail, out.BlockedRoutedTo = threadAgentBlockedOn(snapshot)
		}
		out.Activity = v.app.threadAgentActivity(row.TargetThreadID)
	}
	return out
}

// attachLastMessages reads who spoke last in every local target at once.
func (v threadAgentView) attachLastMessages(rows []ThreadAgentRow) {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ComputerID == "" && row.ThreadID != "" {
			ids = append(ids, row.ThreadID)
		}
	}
	if len(ids) == 0 {
		return
	}
	messages, err := v.app.store.ListThreadLastMessages(ids)
	if err != nil {
		// The line is a convenience beside the row; the row stands.
		log.Printf("agent rows: read last messages: %v", err)
		return
	}
	for index := range rows {
		message, ok := messages[rows[index].ThreadID]
		if !ok || rows[index].ComputerID != "" {
			continue
		}
		rows[index].LastMessage = clipLine(message.Tail)
		rows[index].LastMessageRole = "assistant"
		if message.Kind == "user_text" {
			rows[index].LastMessageRole = "user"
		}
	}
}

// threadAgentBlockedOn says what a target is waiting on a person for: its
// first approval, else its first question.
func threadAgentBlockedOn(snapshot triage.LiveStateSnapshot) (on, detail, routedTo string) {
	if approvals := snapshot.Interactive.Approvals; len(approvals) > 0 {
		request := approvals[0]
		detail = request.ToolName
		// A provider titles a plain tool prompt with the tool's name.
		title := request.Title
		if strings.TrimSpace(title) == request.ToolName {
			title = ""
		}
		if text := firstNonEmpty(title, request.Description); text != "" {
			detail = request.ToolName + ": " + text
		}
		if request.RoutedTo != nil {
			routedTo = request.RoutedTo.Title
		}
		return "approval", clipLine(detail), routedTo
	}
	if questions := snapshot.Interactive.UserInputs; len(questions) > 0 {
		request := questions[0]
		detail = request.Title
		if len(request.Questions) > 0 {
			detail = firstNonEmpty(request.Questions[0].Question, request.Questions[0].Header, detail)
		}
		if request.RoutedTo != nil {
			routedTo = request.RoutedTo.Title
		}
		return "question", clipLine(detail), routedTo
	}
	return "", "", ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// clipLine is one line of text bounded for a row.
func clipLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= threadAgentLineMax {
		return text
	}
	runes := []rune(text)
	return string(runes[:threadAgentLineMax]) + "…"
}

// threadAgentWakeItem is the caller's timeline row that delivered a
// settled request's answer, once its provider confirmed it, or empty.
func (a *App) threadAgentWakeItem(row store.ThreadRequest) string {
	late := row.LateDeliveredAt > 0 && row.LateConfirmedAt > 0
	if row.DeliveredHow != store.ThreadWakeQueued || (!late && row.ConfirmedAt == 0) {
		return ""
	}
	record, found, err := a.findRecordedSend(row.CallerThreadID, threadWakeSendIDFor(row.Token, late))
	if err != nil {
		log.Printf("agent rows: find the wake of %s: %v", row.Token, err)
		return ""
	}
	if !found || !record.dispatched {
		return ""
	}
	return record.item.ID
}

// parkedRequestTokens lists the tokens a tool call is parked on.
func (a *App) parkedRequestTokens() []string {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, wait := range a.threadRequests.waits {
		if wait.remote {
			continue
		}
		for key := range wait.keys {
			if !strings.HasPrefix(key, threadWatchKey("")) && !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out
}

// StopThreadAgentRow stops one Agents row of the thread: it cancels the
// request, as thread_cancel does, with the caller's wake left armed so its
// agent learns a person stopped it. When the row's caller spawned the target,
// the Stop cascades into it as stopping a provider subagent does: its
// Backgrounded items, its running turn, and its own Agents rows, each by the
// same rule, on whichever computer it runs (agent_stop). A computer that
// predates agent_stop has its request cancelled and its turn interrupted,
// and the result names it in Failures. A nested row a paired computer
// listed is stopped through that computer. A row that had already settled
// reports ended.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentRow(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	return a.stopThreadAgentRowIn(ctx, threadID, token)
}

// stopThreadAgentRowIn stops the row of threadID's listing with token: here,
// or through the paired computer that listed it, which finds it the same way
// under the thread the spawn made there.
func (a *App) stopThreadAgentRowIn(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	listing, entries, err := a.threadAgentTree(ctx, threadID)
	if err != nil {
		return ThreadAgentStop{}, err
	}
	for index, row := range listing.Rows {
		if row.Token != token {
			continue
		}
		entry := entries[index]
		if entry.request != nil {
			return a.stopThreadAgentRequest(ctx, *entry.request, map[string]bool{threadID: true}), nil
		}
		stop, err := a.peerThreadAgentStop(ctx, entry.peer, entry.peerSource, entry.peerRoot, token)
		if err != nil {
			return ThreadAgentStop{Token: token, Outcome: threadAgentFailed, Failures: []string{err.Error()}}, nil
		}
		stop.Token = token
		return stop, nil
	}
	// Not listed: settled past the window, or never this thread's.
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		return ThreadAgentStop{}, err
	}
	if found && threadRequestSettled(row) {
		return ThreadAgentStop{Token: token, Outcome: threadAgentEnded}, nil
	}
	return ThreadAgentStop{}, errorsx.Public(threadtools.CodeRequestUnknown,
		"That agent is not one this thread is waiting on.", nil)
}

// StopThreadAgentRows stops every Agents row of the thread as
// StopThreadAgentRow stops one, and reports each top-level row with what
// its cascade stopped beneath it, then withdraws the incoming messages
// queued for the thread as StopThreadAgentIncoming does, each reported.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentRows(ctx context.Context, threadID string) ([]ThreadAgentStop, error) {
	listing, entries, err := a.threadAgentTree(ctx, threadID)
	if err != nil {
		return nil, err
	}
	var top []store.ThreadRequest
	for index, row := range listing.Rows {
		if row.Depth == 0 && entries[index].request != nil {
			top = append(top, *entries[index].request)
		}
	}
	// The incoming rows are the ones shown now: the wakes the Stops below
	// queue tell the agent what the person stopped, and are not withdrawn.
	incoming := a.incomingAgentSendIDs(threadID)
	out := make([]ThreadAgentStop, len(top))
	runBounded(len(top), threadAgentStopsInFlight, func(index int) {
		out[index] = a.stopThreadAgentRequest(ctx, top[index], map[string]bool{threadID: true})
	})
	for _, sendID := range incoming {
		out = append(out, a.withdrawIncomingAgentMessage(ctx, threadID, sendID))
	}
	return out, nil
}

// stopThreadAgentRequest stops one request for a person and, for a spawn,
// what the spawned thread is running, wherever it runs. visited holds the
// threads this Stop already reached here, so a spawn cycle ends.
func (a *App) stopThreadAgentRequest(ctx context.Context, row store.ThreadRequest, visited map[string]bool) ThreadAgentStop {
	result := ThreadAgentStop{Token: row.Token, Outcome: threadAgentStopped}
	current, found, err := a.store.GetThreadRequest(row.Token)
	if err != nil {
		result.Outcome = threadAgentFailed
		result.Failures = append(result.Failures, "read the request: "+err.Error())
		return result
	}
	if !found {
		result.Outcome = threadAgentEnded
		return result
	}
	settled := threadRequestSettled(current)
	if settled {
		result.Outcome = threadAgentEnded
	}
	spawned := current.Kind == store.ThreadRequestSpawn && current.TargetThreadID != ""
	if current.TargetComputerID != "" {
		a.stopRemoteAgentRequest(ctx, current, settled, spawned, &result)
		return result
	}
	if !settled {
		effect, err := a.stopThreadRequestWorkFor(ctx, current.Token, threadRequestUserStop)
		if err != nil {
			result.Outcome = threadAgentFailed
			result.Failures = append(result.Failures, "cancel the request: "+err.Error())
		}
		result.Interrupted = effect == threadtools.EffectInterrupted
	}
	// Only the caller's own spawn: a thread it sent to belongs to someone
	// else, and the cancel above already stopped the turn the request
	// started there.
	if !spawned || visited[current.TargetThreadID] {
		return result
	}
	visited[current.TargetThreadID] = true
	a.stopThreadAgentTarget(ctx, current.TargetThreadID, visited, &result)
	return result
}

// stopRemoteAgentRequest is stopThreadAgentRequest for a target on a paired
// computer: the request and the turn through thread_cancel, and for a
// spawn the cascade there through agent_stop.
func (a *App) stopRemoteAgentRequest(ctx context.Context, current store.ThreadRequest, settled, spawned bool, result *ThreadAgentStop) {
	tools := a.threadTools()
	caller := a.threadAgentCaller(current.CallerThreadID)
	if !settled {
		report, err := tools.cancelRemoteRequest(ctx, caller, current, true)
		if err != nil {
			result.Outcome = threadAgentFailed
			result.Failures = append(result.Failures, "cancel the request: "+err.Error())
		}
		result.Interrupted = report.Effect == threadtools.EffectInterrupted
	}
	if !spawned {
		return
	}
	if !settled {
		report, err := tools.cancelRemoteThread(ctx, caller, threadtools.CancelCall{
			ThreadID: current.TargetThreadID, ComputerID: current.TargetComputerID,
		})
		if err != nil {
			result.Failures = append(result.Failures, "interrupt its turn: "+err.Error())
		} else if report.Effect == threadtools.EffectInterrupted {
			result.Interrupted = true
		}
	}
	remote, err := a.peerThreadAgentStop(ctx, current.TargetComputerID, current.CallerThreadID, current.TargetThreadID, "")
	if err != nil {
		result.Failures = append(result.Failures, "stop its work: "+err.Error())
		return
	}
	result.Interrupted = result.Interrupted || remote.Interrupted
	result.Background += remote.Background
	result.Nested += remote.Nested
	result.Failures = append(result.Failures, remote.Failures...)
}

// stopThreadAgentTarget stops what a spawned thread on this computer is
// running: its Backgrounded items, as a provider subagent's Stop stops the
// tasks it owns, then its turn, then the threads it is waiting on, each by
// the row rule.
func (a *App) stopThreadAgentTarget(ctx context.Context, target string, visited map[string]bool, result *ThreadAgentStop) {
	stopped, err := a.StopThreadBackgroundWork(ctx, target)
	result.Background += stopped
	if err != nil {
		result.Failures = append(result.Failures, "stop its background work: "+err.Error())
	}
	if effect, err := a.threadTools().interruptRunningThread(ctx, target); err != nil {
		result.Failures = append(result.Failures, "interrupt its turn: "+err.Error())
	} else if effect == threadtools.EffectInterrupted {
		result.Interrupted = true
	}
	nested, _, err := a.store.ListThreadAgentRequests(target, a.parkedRequestTokens(), time.Now().Add(-threadAgentSettledWindow).UnixMilli(), threadAgentRowLimit)
	if err != nil {
		result.Failures = append(result.Failures, "list its agents: "+err.Error())
		return
	}
	for _, child := range nested {
		if threadRequestSettled(child) && child.Kind != store.ThreadRequestSpawn {
			continue
		}
		stop := a.stopThreadAgentRequest(ctx, child, visited)
		if stop.Outcome == threadAgentStopped {
			result.Nested++
		}
		result.Nested += stop.Nested
		result.Background += stop.Background
		result.Failures = append(result.Failures, stop.Failures...)
	}
}

// threadAgentCaller names a local caller thread to a paired computer, as a
// forwarded thread_cancel does.
func (a *App) threadAgentCaller(threadID string) threadtools.Caller {
	backendID, _ := a.backendIdentity()
	caller := threadtools.Caller{ThreadID: threadID, ComputerID: backendID, ComputerName: a.backendDisplayName()}
	if thread, err := a.store.GetThread(threadID); err == nil {
		caller.Title = thread.Title
	}
	return caller
}

// threadAgentEmits coalesces what changed into frames, off the goroutines
// that reported it: a store write, an emit, a wait.
type threadAgentEmits struct {
	mu       sync.Mutex
	tokens   map[string]struct{}
	callers  map[string]struct{}
	targets  map[string]struct{}
	draining bool
	// activity is each running target's latest activity line, while it has
	// a request running; lastActivity and activityDue throttle the frames
	// activity alone emits.
	activity     map[string]string
	lastActivity map[string]time.Time
	activityDue  map[string]bool
}

// noteThreadRequestChanged is the store's change hook.
func (a *App) noteThreadRequestChanged(change store.ThreadRequestChange) {
	if change.Token == "" {
		a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.callers[change.CallerThreadID] = struct{}{} })
		return
	}
	a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.tokens[change.Token] = struct{}{} })
}

// noteThreadAgentTarget reports a change to a thread some request runs on.
func (a *App) noteThreadAgentTarget(threadID string) {
	if threadID == "" {
		return
	}
	if _, running := a.runningReceiptTokens(threadID); !running {
		return
	}
	a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.targets[threadID] = struct{}{} })
}

func (a *App) queueThreadAgentEmit(add func(*threadAgentEmits)) {
	e := &a.threadAgentEmits
	e.mu.Lock()
	if e.tokens == nil {
		e.tokens, e.callers, e.targets = map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	}
	add(e)
	if e.draining || a.lifeCtx().Err() != nil {
		e.mu.Unlock()
		return
	}
	e.draining = true
	e.mu.Unlock()
	a.threadRequestsWG.Add(1)
	go func() {
		defer a.threadRequestsWG.Done()
		a.drainThreadAgentEmits()
	}()
}

func (a *App) drainThreadAgentEmits() {
	e := &a.threadAgentEmits
	for {
		e.mu.Lock()
		tokens, callers, targets := e.tokens, e.callers, e.targets
		if len(tokens)+len(callers)+len(targets) == 0 || a.lifeCtx().Err() != nil {
			e.draining = false
			e.mu.Unlock()
			return
		}
		e.tokens, e.callers, e.targets = map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
		e.mu.Unlock()
		for threadID := range targets {
			for _, token := range a.threadAgentTargetTokens(threadID) {
				tokens[token] = struct{}{}
			}
		}
		for caller := range callers {
			a.emit(eventchan.ThreadAgentRowsChanged, ThreadAgentRowChange{ThreadID: caller})
		}
		view := newThreadAgentView(a)
		for token := range tokens {
			a.emitThreadAgentRow(view, token)
		}
	}
}

// threadAgentTargetTokens lists the open requests this computer's threads
// made of a target.
func (a *App) threadAgentTargetTokens(threadID string) []string {
	receipts, err := a.store.ListThreadRequestReceiptsForThread(threadID)
	if err != nil {
		log.Printf("agent rows: list the requests of %s: %v", threadID, err)
		return nil
	}
	var out []string
	for _, receipt := range receipts {
		if receipt.OwnerDeviceID != threadReceiptLocalOwner {
			continue
		}
		if receipt.State == store.ThreadReceiptAccepted || receipt.State == store.ThreadReceiptRunning {
			out = append(out, receipt.Token)
		}
	}
	return out
}

func (a *App) emitThreadAgentRow(view threadAgentView, token string) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		log.Printf("agent rows: read %s for its change event: %v", token, err)
		return
	}
	if !found || row.Kind == store.ThreadRequestRemind {
		return
	}
	change := ThreadAgentRowChange{ThreadID: row.CallerThreadID, ID: token}
	listed := row.Kind == store.ThreadRequestSpawn || row.Notify || a.requestWaitActive(token)
	if listed {
		current := []ThreadAgentRow{view.row(row, 0)}
		view.attachLastMessages(current)
		change.Row = &current[0]
	} else {
		change.Removed = true
	}
	a.emit(eventchan.ThreadAgentRowsChanged, change)
}

// tapThreadAgentRows is the emit tap: a running target's turn, prompt,
// pending send or title changed, or it did something new.
func (a *App) tapThreadAgentRows(name eventchan.Channel, data any) {
	switch name {
	case eventchan.ProviderTurnStarted:
		if event, ok := data.(triage.TurnStartedEvent); ok {
			a.noteThreadAgentTarget(event.ThreadID)
		}
	case eventchan.ProviderTurnCompleted:
		if event, ok := data.(triage.TurnCompletedEvent); ok {
			a.forgetThreadAgentActivity(event.ThreadID)
			a.noteThreadAgentTarget(event.ThreadID)
		}
	case eventchan.ProviderSendsPending:
		if event, ok := data.(SendsPendingEvent); ok {
			a.noteThreadAgentTarget(event.ThreadID)
		}
	case eventchan.ProviderApproval:
		if event, ok := data.(provider.ApprovalEvent); ok {
			threadID := event.ThreadID
			if threadID == "" && event.Request != nil {
				threadID = event.Request.ThreadID
			}
			a.noteThreadAgentTarget(threadID)
		}
	case eventchan.ProviderUserInput:
		if event, ok := data.(provider.UserInputEvent); ok {
			threadID := event.ThreadID
			if threadID == "" && event.Request != nil {
				threadID = event.Request.ThreadID
			}
			a.noteThreadAgentTarget(threadID)
		}
	case eventchan.ProviderSessionDied:
		if event, ok := data.(triage.SessionDiedEvent); ok {
			a.forgetThreadAgentActivity(event.ThreadID)
			a.noteThreadAgentTarget(event.ThreadID)
		}
	case eventchan.ThreadUpdated:
		if event, ok := data.(triage.ThreadUpdateEvent); ok && event.Thread != nil {
			a.noteThreadAgentTarget(event.Thread.ID)
		} else if ok && event.Title != nil {
			a.noteThreadAgentTarget(event.ID)
		}
	case eventchan.ProviderItemEvent:
		if event, ok := data.(triage.ItemStreamEvent); ok && event.Item != nil && event.Item.ParentID == "" {
			a.noteThreadAgentActivity(event.ThreadID, *event.Item)
		}
	}
}

// noteThreadAgentActivity records a running target's latest top-level row
// as its activity line, and emits it at most every
// threadAgentActivityEvery.
func (a *App) noteThreadAgentActivity(threadID string, item store.Item) {
	switch item.Kind {
	case "user_text", "thinking", "reasoning":
		return
	}
	line := clipLine(item.Summary)
	if line == "" || threadID == "" {
		return
	}
	if _, running := a.runningReceiptTokens(threadID); !running {
		return
	}
	e := &a.threadAgentEmits
	e.mu.Lock()
	if e.activity == nil {
		e.activity, e.lastActivity, e.activityDue = map[string]string{}, map[string]time.Time{}, map[string]bool{}
	}
	if e.activity[threadID] == line {
		e.mu.Unlock()
		return
	}
	e.activity[threadID] = line
	if e.activityDue[threadID] {
		e.mu.Unlock()
		return
	}
	wait := threadAgentActivityEvery - time.Since(e.lastActivity[threadID])
	e.activityDue[threadID] = true
	e.mu.Unlock()
	emit := func() {
		e.mu.Lock()
		delete(e.activityDue, threadID)
		e.lastActivity[threadID] = time.Now()
		e.mu.Unlock()
		a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.targets[threadID] = struct{}{} })
	}
	if wait <= 0 {
		emit()
		return
	}
	time.AfterFunc(wait, emit)
}

// threadAgentActivity is a target's latest activity line, while it runs.
func (a *App) threadAgentActivity(threadID string) string {
	e := &a.threadAgentEmits
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activity[threadID]
}

// forgetThreadAgentActivity drops a target's line once its turn ends.
func (a *App) forgetThreadAgentActivity(threadID string) {
	e := &a.threadAgentEmits
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.activity, threadID)
	delete(e.lastActivity, threadID)
}

// noteThreadAgentWait reports a call parked on, or leaving, requests: a send
// or ask is an agent while its caller waits on it.
func (a *App) noteThreadAgentWait(keys []string) {
	for _, key := range keys {
		if key == "" || strings.HasPrefix(key, threadWatchKey("")) {
			continue
		}
		a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.tokens[key] = struct{}{} })
	}
}

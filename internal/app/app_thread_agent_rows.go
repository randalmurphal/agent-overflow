package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

// ThreadAgentStop is what stopping one row did: stopped, ended (it had
// already settled) or failed, with the reason.
type ThreadAgentStop struct {
	Token   string `json:"token"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// ListThreadAgentRows lists the requests a thread made of other threads that
// it is waiting on, with their nested rows.
//
//ao:scope threads:read
func (a *App) ListThreadAgentRows(threadID string) (ThreadAgentRows, error) {
	if _, err := a.store.GetThread(threadID); err != nil {
		return ThreadAgentRows{}, fmt.Errorf("read the thread: %w", err)
	}
	listing, _, err := a.threadAgentTree(threadID)
	return listing, err
}

// threadAgentTree reads the listing and the request rows behind it.
func (a *App) threadAgentTree(threadID string) (ThreadAgentRows, []store.ThreadRequest, error) {
	view := newThreadAgentView(a)
	out := ThreadAgentRows{Rows: []ThreadAgentRow{}}
	var requests []store.ThreadRequest
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
			requests = append(requests, row)
			child := row.TargetThreadID
			if row.Kind != store.ThreadRequestSpawn || row.TargetComputerID != "" || child == "" || visited[child] {
				continue
			}
			visited[child] = true
			if depth+1 >= threadAgentRowDepth {
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
	return out, requests, nil
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
	switch {
	case settled:
	case row.TargetComputerID != "":
		if state.State == threadtools.RequestBlocked {
			out.BlockedOn = "person"
		}
	case row.TargetThreadID != "":
		if thread, err := v.app.store.GetThread(row.TargetThreadID); err == nil {
			out.Provider = thread.Provider
		}
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
		if text := firstNonEmpty(request.Title, request.Description); text != "" {
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
// agent learns a person stopped it; and when the row's caller spawned the
// target, it interrupts the target's running turn. A row that had already
// settled reports ended.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentRow(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	listing, requests, err := a.threadAgentTree(threadID)
	if err != nil {
		return ThreadAgentStop{}, err
	}
	for index, row := range listing.Rows {
		if row.Token == token {
			return a.stopThreadAgentRequest(ctx, requests[index]), nil
		}
	}
	// Not listed: settled past the window, or never this thread's.
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		return ThreadAgentStop{}, err
	}
	if found && threadRequestSettled(row) {
		return ThreadAgentStop{Token: token, Outcome: threadAgentEnded}, nil
	}
	return ThreadAgentStop{}, errors.New("that agent is not one this thread is waiting on")
}

// StopThreadAgentRows stops every open Agents row of the thread, nested rows
// included, as StopThreadAgentRow stops one, and reports each.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentRows(ctx context.Context, threadID string) ([]ThreadAgentStop, error) {
	_, requests, err := a.threadAgentTree(threadID)
	if err != nil {
		return nil, err
	}
	open := make([]store.ThreadRequest, 0, len(requests))
	for _, row := range requests {
		if !threadRequestSettled(row) {
			open = append(open, row)
		}
	}
	out := make([]ThreadAgentStop, len(open))
	slots := make(chan struct{}, threadAgentStopsInFlight)
	var wg sync.WaitGroup
	for index, row := range open {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			out[index] = a.stopThreadAgentRequest(ctx, row)
		}()
	}
	wg.Wait()
	return out, nil
}

// stopThreadAgentRequest cancels one request for a person and, for a spawn,
// interrupts the thread it made.
func (a *App) stopThreadAgentRequest(ctx context.Context, row store.ThreadRequest) ThreadAgentStop {
	result := ThreadAgentStop{Token: row.Token, Outcome: threadAgentStopped}
	current, found, err := a.store.GetThreadRequest(row.Token)
	if err != nil {
		return threadAgentStopFailed(row.Token, err)
	}
	if !found || threadRequestSettled(current) {
		result.Outcome = threadAgentEnded
		return result
	}
	tools := a.threadTools()
	if current.TargetComputerID != "" {
		caller := a.threadAgentCaller(current.CallerThreadID)
		if _, err := tools.cancelRemoteRequest(ctx, caller, current, true); err != nil {
			return threadAgentStopFailed(row.Token, err)
		}
		if current.Kind == store.ThreadRequestSpawn && current.TargetThreadID != "" {
			if _, err := tools.cancelRemoteThread(ctx, caller, threadtools.CancelCall{
				ThreadID: current.TargetThreadID, ComputerID: current.TargetComputerID,
			}); err != nil {
				return threadAgentStopFailed(row.Token, err)
			}
		}
		return result
	}
	if _, err := a.stopThreadRequestWorkFor(ctx, current.Token, threadRequestUserStop); err != nil {
		return threadAgentStopFailed(row.Token, err)
	}
	if current.Kind == store.ThreadRequestSpawn && current.TargetThreadID != "" {
		// Only the caller's own spawn: a thread it sent to belongs to
		// someone else, and the cancel above already stopped the turn the
		// request started there.
		if _, err := tools.interruptRunningThread(ctx, current.TargetThreadID); err != nil {
			return threadAgentStopFailed(row.Token, err)
		}
	}
	return result
}

func threadAgentStopFailed(token string, err error) ThreadAgentStop {
	return ThreadAgentStop{Token: token, Outcome: threadAgentFailed, Error: errorMessage(err)}
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

// errorMessage is an error's text for a row.
func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

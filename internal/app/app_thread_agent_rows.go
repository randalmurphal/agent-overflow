package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
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
// prompt, title, latest activity) emits `thread:agent_rows_changed` naming
// the request and its caller. A pane reads the one row again
// (GetThreadAgentRows) on a frame naming a row it lists, and its list again
// on any other frame that names its thread or a thread its list nests.

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
	// threadAgentAnswerHead is how much of an answer a listing reads: one
	// byte past what clipLine keeps, so it clips the head as it would the
	// whole answer.
	threadAgentAnswerHead = threadAgentLineMax*utf8.UTFMax*2 + 1
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
	// Provider is the target's provider: a local target's row, or what a
	// paired computer reported with its acknowledgement or its agent_rows
	// answer.
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
	// NestedUnavailable says why a spawn on another computer shows none of
	// that thread's own agents: the computer did not answer, runs a version
	// that cannot list them, or is no longer paired.
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
	// ThreadProvider is the listed thread's own provider, answered by
	// agent_rows so the spawn row on the computer that asked can show it.
	ThreadProvider string `json:"threadProvider,omitempty"`
}

// ThreadAgentRowChange is one `thread:agent_rows_changed` frame: the request
// ID of ThreadID changed, or, without an ID, any request of the thread.
// Whether and how the request is listed now is GetThreadAgentRows' answer.
type ThreadAgentRowChange struct {
	ThreadID string `json:"threadId"`
	ID       string `json:"id,omitempty"`
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
// included. It reads this computer's ledger only: a spawn on a paired
// computer nests that computer's last answer, asked again in the
// background, and a changed answer arrives as a change frame.
//
//ao:scope threads:read
func (a *App) ListThreadAgentRows(ctx context.Context, threadID string) (ThreadAgentRows, error) {
	if _, err := a.store.GetThread(threadID); err != nil {
		return ThreadAgentRows{}, fmt.Errorf("read the thread: %w", err)
	}
	listing, _, err := a.threadAgentTreeDepth(ctx, threadID, threadAgentRowDepth, threadAgentPeersCached)
	return listing, err
}

// ThreadRequestState is one request a thread made as the ledger holds it:
// its state, the caller's timeline row that delivered its answer once the
// provider confirmed that row, whether or not any pane has it loaded, and
// whether the answer has reached the caller at all.
type ThreadRequestState struct {
	Token      string `json:"token"`
	State      string `json:"state"`
	WakeItemID string `json:"wakeItemId,omitempty"`
	// Answered is set once the answer reached the caller: in its confirmed
	// wake row, or in the reply of a call that collected it (a thread_status
	// or a listing), which is the delivery and leaves no wake row.
	Answered bool `json:"answered"`
}

// maxThreadRequestStates bounds one GetThreadRequestStates read.
const maxThreadRequestStates = 256

// GetThreadRequestStates reads the requests among tokens that threadID made,
// for the tool rows that made them, whether the Agents listing still shows
// them or not. A token threadID did not make is left out.
//
//ao:scope threads:read
func (a *App) GetThreadRequestStates(threadID string, tokens []string) ([]ThreadRequestState, error) {
	if len(tokens) > maxThreadRequestStates {
		return nil, fmt.Errorf("read request states: %d tokens, at most %d", len(tokens), maxThreadRequestStates)
	}
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		if token != "" && !seen[token] {
			seen[token] = true
			unique = append(unique, token)
		}
	}
	deliveries, err := a.store.ThreadRequestDeliveries(threadID, unique)
	if err != nil {
		return nil, fmt.Errorf("read request states: %w", err)
	}
	out := make([]ThreadRequestState, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, ThreadRequestState{
			Token: d.Token, State: d.State, WakeItemID: d.WakeItemID,
			Answered: d.WakeItemID != "" || threadRequestDeliveredInline(d),
		})
	}
	return out, nil
}

// ThreadAgentRowRead is one request as threadID's Agents listing shows it
// now. Listed is false, with no row, for a request the listing does not
// show: one it never listed, one no longer an agent, or one past the
// settled window.
type ThreadAgentRowRead struct {
	Token  string          `json:"token"`
	Row    *ThreadAgentRow `json:"row,omitempty"`
	Listed bool            `json:"listed"`
}

// maxThreadAgentRowReads bounds one GetThreadAgentRows read: a pane with
// more rows to read than this reads its list instead.
const maxThreadAgentRowReads = 32

// GetThreadAgentRows reads rows of threadID's Agents listing by their
// tokens, in the order asked: each a request of threadID or of a thread
// its spawns made here, within the listing's depth. A row stands on its
// own: what nests under it, and a spawn on a paired computer's rows, are
// the listing's. The read is as many statements as the listing's, over the
// named rows alone: their places in the lineage, the rows, and their
// targets' names and last messages.
//
//ao:scope threads:read
func (a *App) GetThreadAgentRows(threadID string, tokens []string) ([]ThreadAgentRowRead, error) {
	if len(tokens) == 0 || len(tokens) > maxThreadAgentRowReads {
		return nil, fmt.Errorf("read agent rows: %d tokens, 1 to %d", len(tokens), maxThreadAgentRowReads)
	}
	places, err := a.store.ThreadAgentRowPlaces(threadID, tokens, threadAgentRowDepth)
	if err != nil {
		return nil, fmt.Errorf("read agent rows: %w", err)
	}
	depths := make(map[string]int, len(places))
	placed := make([]string, 0, len(places))
	for _, place := range places {
		depths[place.Token] = place.Depth
		placed = append(placed, place.Token)
	}
	view := newThreadAgentView(a)
	var requests []store.ThreadAgentRequest
	if len(placed) > 0 {
		if requests, _, err = view.tree(threadID, 1, placed, len(placed)); err != nil {
			return nil, fmt.Errorf("read agent rows: %w", err)
		}
	}
	rows, err := view.rows(requests)
	if err != nil {
		return nil, fmt.Errorf("read agent rows: %w", err)
	}
	found := make(map[string]ThreadAgentRow, len(rows))
	for _, row := range rows {
		row.Depth, row.NestedHidden = depths[row.Token], false
		found[row.Token] = row
	}
	out := make([]ThreadAgentRowRead, 0, len(tokens))
	for _, token := range tokens {
		read := ThreadAgentRowRead{Token: token}
		if row, ok := found[token]; ok {
			read.Row, read.Listed = &row, true
		}
		out = append(out, read)
	}
	return out, nil
}

// threadAgentPeerMode is how a listing reads a spawn's rows on a paired
// computer.
type threadAgentPeerMode int

const (
	// threadAgentPeersCached nests the last answer and asks again in the
	// background: a pane's read.
	threadAgentPeersCached threadAgentPeerMode = iota
	// threadAgentPeersLive asks each computer now, concurrently: an
	// agent_rows answer for another computer, which asked off its pane's
	// path.
	threadAgentPeersLive
)

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

// threadAgentRemoteSlot is a spawn on a paired computer in a listing: its
// row's index and the depth its nested rows start at.
type threadAgentRemoteSlot struct {
	index int
	depth int
	spawn store.ThreadRequest
}

// threadAgentTreeDepth reads a listing nested at most maxDepth levels, and
// what stops each row: a paired computer listing for a caller's nested row
// has fewer left.
func (a *App) threadAgentTreeDepth(ctx context.Context, threadID string, maxDepth int, peers threadAgentPeerMode) (ThreadAgentRows, []threadAgentEntry, error) {
	view := newThreadAgentView(a)
	requests, more, err := view.tree(threadID, maxDepth, nil, threadAgentRowLimit)
	if err != nil {
		return ThreadAgentRows{}, nil, err
	}
	rows, err := view.rows(requests)
	if err != nil {
		return ThreadAgentRows{}, nil, err
	}
	out := ThreadAgentRows{Rows: rows, Hidden: more}
	entries := make([]threadAgentEntry, 0, len(requests))
	var remote []threadAgentRemoteSlot
	for index := range requests {
		request := &requests[index].ThreadRequest
		entries = append(entries, threadAgentEntry{request: request})
		depth := requests[index].Depth
		if request.Kind == store.ThreadRequestSpawn && request.TargetThreadID != "" && request.TargetComputerID != "" && depth+1 < maxDepth {
			remote = append(remote, threadAgentRemoteSlot{index: index, depth: depth + 1, spawn: *request})
		}
	}
	answers := a.remoteAgentAnswers(ctx, remote, maxDepth, peers)
	// Spliced from the last, so each slot's index still names its row.
	for i := len(remote) - 1; i >= 0; i-- {
		rows, rowEntries := a.nestPeerAgentRows(&out.Rows[remote[i].index], remote[i], answers[i], maxDepth)
		at := remote[i].index + 1
		out.Rows = append(out.Rows[:at], append(rows, out.Rows[at:]...)...)
		entries = append(entries[:at], append(rowEntries, entries[at:]...)...)
		out.Hidden = out.Hidden || answers[i].listing.Hidden
	}
	if len(out.Rows) > threadAgentRowLimit {
		out.Rows, entries, out.Hidden = out.Rows[:threadAgentRowLimit], entries[:threadAgentRowLimit], true
	}
	return out, entries, nil
}

// remoteAgentAnswers reads each remote spawn's rows: the last answer, with
// another asked in the background, or every computer now, concurrently. A
// spawn whose computer is no longer paired is asked nothing and answers
// that it is not.
func (a *App) remoteAgentAnswers(ctx context.Context, remote []threadAgentRemoteSlot, maxDepth int, peers threadAgentPeerMode) []threadAgentPeerAnswer {
	answers := make([]threadAgentPeerAnswer, len(remote))
	pairing := map[string]error{}
	ask := make([]int, 0, len(remote))
	for i, slot := range remote {
		computer := slot.spawn.TargetComputerID
		err, seen := pairing[computer]
		if !seen {
			var paired bool
			if paired, err = a.peerPaired(computer); err == nil && !paired {
				err = errThreadAgentPeerUnpaired
			}
			pairing[computer] = err
		}
		if err != nil {
			answers[i] = threadAgentPeerAnswer{err: err}
			continue
		}
		ask = append(ask, i)
	}
	if peers == threadAgentPeersLive {
		runBounded(len(ask), threadAgentPeerListsInFlight, func(n int) {
			i := ask[n]
			call, cancel := context.WithTimeout(ctx, threadAgentPeerServeTimeout)
			defer cancel()
			listing, err := a.peerThreadAgentRows(call, remote[i].spawn.TargetComputerID, remote[i].spawn.MadeIn(),
				remote[i].spawn.TargetThreadID, maxDepth-remote[i].depth)
			a.notePeerAgentRows(remote[i].spawn, listing, err)
			answers[i] = threadAgentPeerAnswer{listing: listing, err: err}
		})
		return answers
	}
	for _, i := range ask {
		if answer, ok := a.cachedPeerAgentRows(remote[i].spawn); ok {
			answers[i] = answer
		}
		a.refreshPairedPeerAgentRows(remote[i].spawn, threadAgentPeerRefreshEvery)
	}
	return answers
}

// nestPeerAgentRows renders a paired computer's answer as the rows nested
// under its spawn's row, named by that computer, deeper than the bound left
// out with their parent marked. A computer that did not answer leaves the
// reason on the spawn's row.
func (a *App) nestPeerAgentRows(parent *ThreadAgentRow, slot threadAgentRemoteSlot, answer threadAgentPeerAnswer, maxDepth int) ([]ThreadAgentRow, []threadAgentEntry) {
	if answer.err != nil {
		parent.NestedUnavailable = threadAgentPeerUnavailable(parent.Computer, answer.err)
		return nil, nil
	}
	if parent.Provider == "" {
		parent.Provider = answer.listing.ThreadProvider
	}
	self, _ := a.backendIdentity()
	rows := make([]ThreadAgentRow, 0, len(answer.listing.Rows))
	entries := make([]threadAgentEntry, 0, len(answer.listing.Rows))
	for _, row := range answer.listing.Rows {
		row.Depth += slot.depth
		if row.Depth >= maxDepth {
			for i := len(rows) - 1; i >= 0; i-- {
				if rows[i].Depth == row.Depth-1 {
					rows[i].NestedHidden = true
					break
				}
			}
			continue
		}
		row.OwnerComputerID = slot.spawn.TargetComputerID
		switch row.ComputerID {
		case "":
			// A thread on that computer.
			row.ComputerID, row.Computer = slot.spawn.TargetComputerID, parent.Computer
		case self:
			// A thread that computer reached back here on.
			row.ComputerID, row.Computer = "", ""
		}
		rows = append(rows, row)
		entries = append(entries, threadAgentEntry{
			peer: slot.spawn.TargetComputerID, peerSource: slot.spawn.CallerThreadID, peerRoot: slot.spawn.TargetThreadID,
		})
	}
	return rows, entries
}

// threadAgentPeerUnavailable words, for the tray, why a spawn on a paired
// computer shows none of that thread's own agents. The error itself goes to
// the log.
func threadAgentPeerUnavailable(computer string, err error) string {
	if computer == "" {
		computer = "That computer"
	}
	if errors.Is(err, errThreadAgentPeerOlder) {
		return computer + " runs an older version: its agents are not shown or stopped from here"
	}
	if errors.Is(err, errThreadAgentPeerUnpaired) {
		return computer + " is no longer paired"
	}
	log.Printf("agent rows: %s did not list its agents: %v", computer, err)
	return computer + " did not answer; its agents are not shown"
}

// threadAgentView renders request rows for one read: the parked waits and
// the settled window are read once for the whole listing.
type threadAgentView struct {
	app     *App
	waiting []string
	since   int64
}

func newThreadAgentView(a *App) threadAgentView {
	return threadAgentView{
		app: a, waiting: a.parkedRequestTokens(),
		since: time.Now().Add(-threadAgentSettledWindow).UnixMilli(),
	}
}

// tree reads root's requests nested at most depth levels, or only its own
// request token when one is named.
func (v threadAgentView) tree(root string, depth int, tokens []string, limit int) ([]store.ThreadAgentRequest, bool, error) {
	return v.app.store.ListThreadAgentTree(store.ThreadAgentTreeRead{
		Root: root, Waiting: v.waiting, SettledSince: v.since, Depth: depth, OpenDepth: threadAgentRowDepth,
		Limit: limit, AnswerHead: threadAgentAnswerHead, Tokens: tokens,
	})
}

// rows renders requests as their rows, with the threads they went to read
// once for them all.
func (v threadAgentView) rows(requests []store.ThreadAgentRequest) ([]ThreadAgentRow, error) {
	ids := make([]string, 0, len(requests))
	for _, request := range requests {
		if request.TargetComputerID == "" && request.TargetThreadID != "" {
			ids = append(ids, request.TargetThreadID)
			if request.OriginThreadID != "" {
				ids = append(ids, request.OriginThreadID)
			}
		}
	}
	names, err := v.app.store.ThreadNames(ids)
	if err != nil {
		return nil, err
	}
	out := make([]ThreadAgentRow, 0, len(requests))
	for _, request := range requests {
		out = append(out, v.row(request, names))
	}
	v.attachLastMessages(out)
	return out, nil
}

func (v threadAgentView) row(request store.ThreadAgentRequest, names map[string]store.ThreadName) ThreadAgentRow {
	row := request.ThreadRequest
	settled := threadRequestSettled(row)
	local := row.TargetComputerID == "" && row.TargetThreadID != ""
	blocked := false
	var snapshot triage.LiveStateSnapshot
	switch {
	case settled || row.TargetThreadID == "":
	case !local:
		blocked = v.app.remoteRequestBlocked(row.Token)
	case v.app.triage != nil:
		snapshot = v.app.triage.LiveStateSnapshotForThread(row.TargetThreadID)
		blocked = threadToolsLiveState(snapshot).Blocked()
	}
	state := requestStateFrom(row, request.AnswerHead, request.HasLateReply, requestTitleFrom(row, names), blocked)
	out := ThreadAgentRow{
		Token: row.Token, CallerThreadID: row.CallerThreadID, Kind: row.Kind,
		ThreadID: row.TargetThreadID, OpenThreadID: row.TargetThreadID,
		ComputerID: row.TargetComputerID, Title: state.Title, State: state.State,
		LateReply: request.HasLateReply, CreatedAt: row.CreatedAt, SettledAt: row.SettledAt,
		Depth: request.Depth, NestedHidden: request.NestedHidden,
	}
	if row.Kind == store.ThreadRequestAsk && row.OriginThreadID != "" {
		// The fork that answers an ask is hidden; the thread it asked about
		// is what a person opens.
		out.OpenThreadID = row.OriginThreadID
	}
	if row.TargetComputerID != "" {
		out.Computer, _ = v.app.threadTools().threadToolsBackendName(row.TargetComputerID)
		out.Provider = row.TargetProvider
	}
	if settled {
		out.Answer = clipLine(state.Answer)
		out.AnswerKind = state.AnswerKind
		out.WakeItemID = request.WakeItemID
	}
	if name, ok := names[row.TargetThreadID]; ok && local {
		out.Provider = name.Provider
	}
	switch {
	case settled:
	case row.TargetComputerID != "":
		if state.State == threadtools.RequestBlocked {
			out.BlockedOn = "person"
		}
	case row.TargetThreadID != "":
		out.BlockedOn, out.BlockedDetail, out.BlockedRoutedTo = threadAgentBlockedOn(snapshot)
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
	messages, err := v.app.store.ListThreadLastMessages(ids, threadtools.FooterStart)
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
// The text is cut to a byte bound before its whitespace is folded, so a
// streamed summary of any length costs the same.
func clipLine(text string) string {
	cut := false
	if limit := threadAgentLineMax * utf8.UTFMax * 2; len(text) > limit {
		end := limit
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		text, cut = text[:end], true
	}
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > threadAgentLineMax {
		return string([]rune(text)[:threadAgentLineMax]) + "…"
	}
	if cut {
		return text + "…"
	}
	return text
}

// threadRequestDeliveredInline reports that a call's reply carried the
// request's answer or its late reply to the caller (requestState,
// claimInlineDelivery). The mark is written once that reply is, so no wake
// row follows it.
func threadRequestDeliveredInline(d store.ThreadRequestDelivery) bool {
	return (d.DeliveredHow == store.ThreadWakeInline && d.DeliveredAt != 0) ||
		(d.LateDeliveredHow == store.ThreadWakeInline && d.LateDeliveredAt != 0)
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
// Backgrounded items, its running turn, and its own Agents rows, on
// whichever computer it runs (agent_stop). A request inside that lineage is
// cancelled with its wake disarmed: the thread that made it was stopped
// too, and a wake would start it again. A computer that predates agent_stop
// has its request cancelled and its turn interrupted, and the result names
// it in Failures. A nested row a paired computer listed is stopped through
// that computer. A row that had already settled reports ended.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentRow(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	return a.stopThreadAgentRowIn(ctx, threadID, token)
}

// stopThreadAgentRowIn stops the row of threadID's listing with token from
// the ledger, without reading the listing: a request here whose caller is
// threadID or a thread its spawns made here, or a row the last answer of
// one of those spawns on a paired computer listed, stopped through that
// computer, which finds it the same way under the thread the spawn made
// there.
func (a *App) stopThreadAgentRowIn(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		return ThreadAgentStop{}, err
	}
	if found {
		mine, err := a.threadAgentLineageHolds(threadID, row.CallerThreadID)
		if err != nil {
			return ThreadAgentStop{}, err
		}
		if mine {
			return a.stopThreadAgentRequest(ctx, row, map[string]bool{threadID: true}, true), nil
		}
	}
	if spawn, ok := a.cachedPeerSpawnFor(token); ok {
		mine, err := a.threadAgentLineageHolds(threadID, spawn.CallerThreadID)
		if err != nil {
			return ThreadAgentStop{}, err
		}
		if mine {
			stop, err := a.peerThreadAgentStop(ctx, spawn.TargetComputerID, spawn.MadeIn(), spawn.TargetThreadID, token)
			if err != nil {
				return ThreadAgentStop{Token: token, Outcome: threadAgentFailed, Failures: []string{err.Error()}}, nil
			}
			stop.Token = token
			return stop, nil
		}
	}
	return ThreadAgentStop{}, errorsx.Public(threadtools.CodeRequestUnknown,
		"That agent is not one this thread is waiting on.", nil)
}

// threadAgentLineageHolds reports whether caller is threadID or a thread
// spawned under it on this computer, within the listing's depth.
func (a *App) threadAgentLineageHolds(threadID, caller string) (bool, error) {
	_, holds, err := a.threadAgentLineageDepth(threadID, caller)
	return holds, err
}

// threadAgentLineageDepth reports how many spawns down from threadID caller
// is, and whether it is within the listing's depth of it at all.
func (a *App) threadAgentLineageDepth(threadID, caller string) (int, bool, error) {
	current := caller
	for depth := range threadAgentRowDepth {
		if current == threadID {
			return depth, true, nil
		}
		thread, err := a.store.GetThread(current)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, false, nil
			}
			return 0, false, err
		}
		if thread.SpawnedByThreadID == "" {
			return 0, false, nil
		}
		current = thread.SpawnedByThreadID
	}
	return threadAgentRowDepth, current == threadID, nil
}

// StopThreadAgentRows stops every Agents row of the thread as
// StopThreadAgentRow stops one, and reports each top-level row with what
// its cascade stopped beneath it, then withdraws the incoming messages
// queued for the thread as StopThreadAgentIncoming does, each reported.
// The rows come from the ledger; the Stops of rows here start at once,
// beside those that wait on a paired computer.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentRows(ctx context.Context, threadID string) ([]ThreadAgentStop, error) {
	listed, _, err := newThreadAgentView(a).tree(threadID, 1, nil, threadAgentRowLimit)
	if err != nil {
		return nil, err
	}
	top := make([]store.ThreadRequest, 0, len(listed))
	for _, request := range listed {
		top = append(top, request.ThreadRequest)
	}
	// The incoming rows are the ones shown now: the wakes the Stops below
	// queue tell the agent what the person stopped, and are not withdrawn.
	incoming := a.incomingAgentSendIDs(threadID)
	out := make([]ThreadAgentStop, len(top))
	var here, there []int
	for index, row := range top {
		if row.TargetComputerID == "" {
			here = append(here, index)
		} else {
			there = append(there, index)
		}
	}
	stopEach := func(indexes []int) {
		runBounded(len(indexes), threadAgentStopsInFlight, func(i int) {
			out[indexes[i]] = a.stopThreadAgentRequest(ctx, top[indexes[i]], map[string]bool{threadID: true}, true)
		})
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopEach(there)
	}()
	stopEach(here)
	wg.Wait()
	for _, sendID := range incoming {
		out = append(out, a.withdrawIncomingAgentMessage(ctx, threadID, sendID))
	}
	return out, nil
}

// stopThreadAgentRequest stops one request for a person and, for a spawn,
// what the spawned thread is running, wherever it runs. pressed is the row
// the person stopped, whose caller's wake stays armed; a request reached
// through the cascade has its wake disarmed first. visited holds the
// threads this Stop already reached here, so a spawn cycle ends.
func (a *App) stopThreadAgentRequest(ctx context.Context, row store.ThreadRequest, visited map[string]bool, pressed bool) ThreadAgentStop {
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
	} else if !pressed && current.Notify {
		if _, err := a.store.SetThreadRequestNotify(current.Token, false); err != nil {
			result.Failures = append(result.Failures, "disarm its wake: "+err.Error())
		}
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
	// The destination's receipts name the thread that made the request,
	// which New Thread, Same Assistant may since have replaced.
	caller := a.threadAgentCaller(current.MadeIn())
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
	remote, err := a.peerThreadAgentStop(ctx, current.TargetComputerID, current.MadeIn(), current.TargetThreadID, "")
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
// the row rule with its wake disarmed.
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
	nested, _, err := newThreadAgentView(a).tree(target, 1, nil, threadAgentRowLimit)
	if err != nil {
		result.Failures = append(result.Failures, "list its agents: "+err.Error())
		return
	}
	for _, listed := range nested {
		child := listed.ThreadRequest
		if threadRequestSettled(child) && child.Kind != store.ThreadRequestSpawn {
			continue
		}
		stop := a.stopThreadAgentRequest(ctx, child, visited, false)
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
		for caller := range callers {
			a.emit(eventchan.ThreadAgentRowsChanged, ThreadAgentRowChange{ThreadID: caller})
		}
		if len(tokens)+len(targets) == 0 {
			continue
		}
		named, err := a.store.ThreadAgentRowCallers(slices.Collect(maps.Keys(tokens)), slices.Collect(maps.Keys(targets)), threadReceiptLocalOwner)
		if err != nil {
			log.Printf("agent rows: name the callers of %d changed requests and %d targets: %v", len(tokens), len(targets), err)
			continue
		}
		for _, changed := range named {
			a.emit(eventchan.ThreadAgentRowsChanged, ThreadAgentRowChange{ThreadID: changed.CallerThreadID, ID: changed.Token})
		}
	}
}

// tapThreadAgentRows is the emit tap: a running target's turn, prompt,
// pending send or title changed, or it did something new.
func (a *App) tapThreadAgentRows(name eventchan.Channel, data any) {
	// A thread row frame is told by its payload, so this reader names no
	// thread:updated channel: only the announcer emits on it.
	if event, ok := data.(triage.ThreadUpdateEvent); ok {
		if event.Thread != nil {
			a.noteThreadAgentTarget(event.Thread.ID)
		} else if event.Title != nil {
			a.noteThreadAgentTarget(event.ID)
		}
		return
	}
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
	if threadID == "" {
		return
	}
	// Every top-level row of every thread passes here: only a thread a
	// request runs on pays for the line.
	if _, running := a.runningReceiptTokens(threadID); !running {
		return
	}
	line := clipLine(item.Summary)
	if line == "" {
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

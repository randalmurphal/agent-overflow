package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
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
// the request and its caller, and again naming each thread whose listing
// nests that caller. A pane reads the one row again (GetThreadAgentRows) on
// a frame naming a row it lists, and its list again on any other frame that
// names its thread or a thread its list nests. A sidebar row's worker
// summary (GetThreadWorkerSummary) is read again on a frame naming its
// thread. Past rows (ListThreadPastAgentRows) are the ledger's settled rows,
// read by page.

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
	// ThreadID is the target; for an ask, the hidden copy that answers it.
	ThreadID string `json:"threadId,omitempty"`
	// OpenThreadID is the thread the row opens: the target, and for an ask
	// its copy only while the ask is open and the copy is there (a local
	// copy is deleted once it answers); empty for an ask after that.
	OpenThreadID string `json:"openThreadId,omitempty"`
	// AskedThreadID is the thread an ask asked about, which the row's title
	// and provider name; empty for every other kind.
	AskedThreadID string `json:"askedThreadId,omitempty"`
	// ComputerID and Computer name a target on a paired computer; both are
	// empty for this one.
	ComputerID string `json:"computerId,omitempty"`
	Computer   string `json:"computer,omitempty"`
	Title      string `json:"title"`
	// Placement and Archived are the placement and archive state of the
	// thread a spawn made, as the computer that holds the thread reported
	// it: this one's store for a local spawn, the agent_rows answer of a
	// paired computer for one there, nested rows included. Empty and false
	// for every other row and from a computer that does not report them.
	// Placement is empty too for a thread that takes none (an assistant's
	// helper, outside every project), so no sidebar row is offered for it.
	Placement string `json:"placement,omitempty"`
	Archived  bool   `json:"archived,omitempty"`
	// Provider is the target's provider: a local target's row, or what a
	// paired computer reported with its acknowledgement or its agent_rows
	// answer.
	Provider string `json:"provider,omitempty"`
	// State is the request state thread_status reports: running, blocked,
	// replied, finished, errored, cancelled, interrupted, expired, refused,
	// and the accepted and unconfirmed states before the work starts.
	State string `json:"state"`
	// Worker is why the thread a spawn made is not at rest, whatever State
	// says: blocked, running, queued, or waiting on what it started
	// (background work, a loop, watch or reminder, a provider timer) or on
	// its own workers, open work at any depth below it; empty while it is
	// at rest, and for every other kind. A spawn whose worker is not at
	// rest is open: one that replied and keeps working shows its reply and
	// this.
	Worker string `json:"worker,omitempty"`
	// BlockedOn is approval or question for a target waiting on a prompt,
	// here or as a paired computer's agent_rows answer reports it, and
	// person for a paired computer's target known only to be blocked;
	// BlockedDetail says what it asks, an approval's only to a reader that
	// may answer it (withholdApprovalDetail), and BlockedRoutedTo names the
	// spawner its prompt went to (ruling 19). BlockedRoutedHere marks a
	// paired computer's prompt routed back to the spawner here, which
	// BlockedRoutedTo then names as this computer does.
	BlockedOn         string `json:"blockedOn,omitempty"`
	BlockedDetail     string `json:"blockedDetail,omitempty"`
	BlockedRoutedTo   string `json:"blockedRoutedTo,omitempty"`
	BlockedRoutedHere bool   `json:"blockedRoutedHere,omitempty"`
	// Activity is what a running target did last: its latest top-level
	// tool call, while the request runs or its worker is not at rest; a
	// waiting worker's is what it waits on. A paired computer reports its
	// spawn's.
	Activity string `json:"activity,omitempty"`
	// ToolCalls, Tokens and RuntimeMs are what the thread a spawn made did
	// over its life (store.ThreadTally): each top-level tool call it
	// executed, once; the tokens it spent itself, as a native agent's row
	// counts its agent's, its own native agents' left to their rows; and
	// the time its turns ran, the open one's so far.
	// TurnOpen says a turn is open, whose time goes on from RuntimeMs as
	// read, and TurnStartedAt which turn: its start on the computer that
	// holds the thread, an identity a later read is compared with, never a
	// time this computer's clock is compared with. Read here, or reported by the paired computer that holds it;
	// zero for every other kind and from a computer that does not report
	// them, and for a paired spawn under Past whose computer has not
	// answered, NestedUnavailable then saying why when it could not.
	ToolCalls     int64 `json:"toolCalls,omitempty"`
	Tokens        int64 `json:"tokens,omitempty"`
	RuntimeMs     int64 `json:"runtimeMs,omitempty"`
	TurnOpen      bool  `json:"turnOpen,omitempty"`
	TurnStartedAt int64 `json:"turnStartedAt,omitempty"`
	// RestUnreported marks a request settled `finished` by a paired
	// computer whose version settles a spawn when its turn ends and cannot
	// report whether the thread came to rest (store.ThreadRequest.RestUnreported).
	RestUnreported bool `json:"restUnreported,omitempty"`
	// LastMessage is who spoke last in a local target and the end of it.
	LastMessage     string `json:"lastMessage,omitempty"`
	LastMessageRole string `json:"lastMessageRole,omitempty"`
	// Answer is the head of a settled request's answer, AnswerKind its kind.
	Answer     string `json:"answer,omitempty"`
	AnswerKind string `json:"answerKind,omitempty"`
	// LateReply is a reply that arrived after the request settled.
	LateReply bool `json:"lateReply,omitempty"`
	// LateReplyExpiredAt is when such a reply reached a paired computer that
	// dropped it before this one collected it
	// (threadtools.RequestState.LateReplyExpiredAt).
	LateReplyExpiredAt int64 `json:"lateReplyExpiredAt,omitempty"`
	CreatedAt          int64 `json:"createdAt"`
	SettledAt          int64 `json:"settledAt,omitempty"`
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
	Title  string           `json:"title,omitempty"`
	Rows   []ThreadAgentRow `json:"rows"`
	Hidden bool             `json:"hidden,omitempty"`
	// ThreadProvider is the listed thread's own provider, answered by
	// agent_rows so the spawn row on the computer that asked can show it.
	ThreadProvider string `json:"threadProvider,omitempty"`
	// Worker is why the listed thread itself is not at rest, answered by
	// agent_rows so the spawn row on the computer that asked is open while
	// it is (ThreadAgentRow.Worker).
	Worker string `json:"worker,omitempty"`
	// Open is whether the listed thread is not at rest or has open work at
	// any depth below it, answered by agent_rows whatever rows it has room
	// for (answerThreadAgentRows); nil only in a listing of this computer's
	// own rows, which no agent_rows answer carries.
	Open *bool `json:"open,omitempty"`
	// Activity, ToolCalls, Tokens, RuntimeMs, TurnOpen and TurnStartedAt are the listed
	// thread's own, answered by agent_rows for the spawn row on the
	// computer that asked (ThreadAgentRow). A computer that predates them
	// sends none.
	Activity      string `json:"activity,omitempty"`
	ToolCalls     int64  `json:"toolCalls,omitempty"`
	Tokens        int64  `json:"tokens,omitempty"`
	RuntimeMs     int64  `json:"runtimeMs,omitempty"`
	TurnOpen      bool   `json:"turnOpen,omitempty"`
	TurnStartedAt int64  `json:"turnStartedAt,omitempty"`
	// Placement and Archived are the listed thread's own, answered by
	// agent_rows for the spawn row on the computer that asked
	// (ThreadAgentRow.Placement).
	Placement string `json:"placement,omitempty"`
	Archived  bool   `json:"archived,omitempty"`
	// BlockedOn, BlockedDetail and BlockedRoutedTo are the prompt the
	// listed thread waits on, as its own Agents row here reads it
	// (threadWaitingOn), an approval's detail withheld from a session that
	// may not answer it (withholdApprovalDetail). RoutedToAsker says that
	// prompt went to the thread on the asking computer whose link owns the
	// listed thread (routedToAsker), which that computer names itself.
	BlockedOn       string `json:"blockedOn,omitempty"`
	BlockedDetail   string `json:"blockedDetail,omitempty"`
	BlockedRoutedTo string `json:"blockedRoutedTo,omitempty"`
	RoutedToAsker   bool   `json:"routedToAsker,omitempty"`
	// Prompts are the listed thread's prompts routed to the asking
	// computer's spawn thread, answered by agent_rows for a call that names
	// the spawn whose link owns the thread (peerPromptsOf).
	Prompts []ThreadAgentPrompt `json:"prompts,omitempty"`
}

// ThreadAgentRowChange is one `thread:agent_rows_changed` frame: the request
// ID that ThreadID made changed, or, without an ID, any request of the
// thread. The frame is sent again for each thread above ThreadID whose
// listing nests its rows, with the same ID. Whether and how the request is
// listed now is GetThreadAgentRows' answer.
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
	listing, _, err := a.threadAgentTreeDepth(ctx, newThreadAgentView(a), threadID, threadAgentRowDepth, threadAgentPeersCached)
	a.withholdApprovalDetail(ctx, &listing)
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
// tokens, in the order asked, each as the listing places it, wherever it
// falls against the listing's row limit. A row stands on its own: what
// nests under it, and a spawn on a paired computer's rows, are the
// listing's. The read takes no more statements than the listing, and reads
// each named row and the spawns above it up to the thread, no other row.
//
//ao:scope threads:read
func (a *App) GetThreadAgentRows(ctx context.Context, threadID string, tokens []string) ([]ThreadAgentRowRead, error) {
	if len(tokens) == 0 || len(tokens) > maxThreadAgentRowReads {
		return nil, fmt.Errorf("read agent rows: %d tokens, 1 to %d", len(tokens), maxThreadAgentRowReads)
	}
	view := newThreadAgentView(a)
	requests, _, err := view.tree(threadID, threadAgentRowDepth, tokens, threadAgentRowLimit)
	if err != nil {
		return nil, fmt.Errorf("read agent rows: %w", err)
	}
	rows, err := view.rows(requests)
	if err != nil {
		return nil, fmt.Errorf("read agent rows: %w", err)
	}
	a.withholdApprovalDetail(ctx, &ThreadAgentRows{Rows: rows})
	found := make(map[string]ThreadAgentRow, len(rows))
	for _, row := range rows {
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

// PastAgentRows is one page of a thread's settled Agents rows, flat
// (depth 0) and newest settled first. Total counts every settled row the
// thread has; Next starts the following page and is absent on the last.
type PastAgentRows struct {
	Rows  []ThreadAgentRow           `json:"rows"`
	Total int                        `json:"total"`
	Next  *store.PastAgentRowsCursor `json:"next,omitempty"`
}

// ListThreadPastAgentRows lists one page of the requests the thread made
// that its Agents segment lists under Past: every spawn and ask it made and
// its sends with their wake armed, once settled and, for a spawn, once the
// thread it made is at rest, as the listing reads it, newest settled first, from
// after the cursor, at most limit (1 to store.MaxPastAgentRowsPage) rows.
// Each row carries what an open row carries, derived the same way; a spawn
// the retention floor reduced to its lineage shows no answer.
//
//ao:scope threads:read
func (a *App) ListThreadPastAgentRows(ctx context.Context, threadID string, after store.PastAgentRowsCursor, limit int) (PastAgentRows, error) {
	if _, err := a.store.GetThread(threadID); err != nil {
		return PastAgentRows{}, fmt.Errorf("read the thread: %w", err)
	}
	view := newThreadAgentView(a)
	page, err := a.store.ListThreadPastAgentRequests(threadID, after, limit, threadAgentAnswerHead, view.waiting, view.live)
	if err != nil {
		return PastAgentRows{}, fmt.Errorf("read past agent rows: %w", err)
	}
	rows, err := view.rows(page.Requests)
	if err != nil {
		return PastAgentRows{}, fmt.Errorf("read past agent rows: %w", err)
	}
	a.pastPeerSpawnCounts(rows, page.Requests)
	a.withholdApprovalDetail(ctx, &ThreadAgentRows{Rows: rows})
	return PastAgentRows{Rows: rows, Total: page.Total, Next: page.Next}, nil
}

// ThreadWorkerSummary is what a thread's sidebar row says of its Agents
// listing: Open counts the threads its open rows went to, nested rows
// included, each once however many requests went to it, and NeedYou those
// of them blocked on a prompt no agent was given.
type ThreadWorkerSummary struct {
	ThreadID string `json:"threadId"`
	Open     int    `json:"open"`
	NeedYou  int    `json:"needYou"`
}

// threadWorkerSummaryPage bounds one page of ListThreadWorkerSummaries:
// each summary reads its thread's listing.
const threadWorkerSummaryPage = 64

// ThreadWorkerSummaryPage is one page of the threads whose Agents listing
// may show an open row, in thread id order. Summaries holds those that
// show one. Next continues after the last thread the page read; it is
// empty on the last page, and only the pages read up to it together say
// which threads have no open row.
type ThreadWorkerSummaryPage struct {
	Summaries []ThreadWorkerSummary `json:"summaries"`
	Next      string                `json:"next,omitempty"`
}

// ListThreadWorkerSummaries summarizes one page of the threads whose
// Agents listing shows an open row, after the thread id `after` (empty for
// the first page), by thread id. A thread with none is left out.
//
//ao:scope threads:read
//ao:route all
func (a *App) ListThreadWorkerSummaries(ctx context.Context, after string) (ThreadWorkerSummaryPage, error) {
	view := newThreadAgentView(a)
	view.counts = true
	ids, more, err := a.store.ThreadsWithOpenAgentRows(view.waiting, view.live, after, threadWorkerSummaryPage)
	if err != nil {
		return ThreadWorkerSummaryPage{}, fmt.Errorf("list worker summaries: %w", err)
	}
	page := ThreadWorkerSummaryPage{Summaries: make([]ThreadWorkerSummary, 0, len(ids))}
	for _, id := range ids {
		summary, err := a.threadWorkerSummary(ctx, view, id)
		if err != nil {
			return ThreadWorkerSummaryPage{}, fmt.Errorf("list worker summaries: %w", err)
		}
		if summary.Open > 0 {
			page.Summaries = append(page.Summaries, summary)
		}
	}
	if more {
		page.Next = ids[len(ids)-1]
	}
	return page, nil
}

// GetThreadWorkerSummary summarizes the thread's Agents listing as it stands
// now: zero counts when it shows no open row, or the thread is gone.
//
//ao:scope threads:read
func (a *App) GetThreadWorkerSummary(ctx context.Context, threadID string) (ThreadWorkerSummary, error) {
	if _, err := a.store.GetThread(threadID); errors.Is(err, sql.ErrNoRows) {
		// Deleted since the frame that asked: it has no open worker.
		return ThreadWorkerSummary{ThreadID: threadID}, nil
	} else if err != nil {
		return ThreadWorkerSummary{}, fmt.Errorf("read the thread: %w", err)
	}
	view := newThreadAgentView(a)
	view.counts = true
	return a.threadWorkerSummary(ctx, view, threadID)
}

// threadWorkerSummary counts the threads the open rows of the thread's
// listing went to, read as a pane reads it, through view, so the counts
// are the rows the pane shows.
func (a *App) threadWorkerSummary(ctx context.Context, view threadAgentView, threadID string) (ThreadWorkerSummary, error) {
	listing, _, err := a.threadAgentTreeDepth(ctx, view, threadID, threadAgentRowDepth, threadAgentPeersCached)
	if err != nil {
		return ThreadWorkerSummary{}, err
	}
	summary := ThreadWorkerSummary{ThreadID: threadID}
	open, needYou := map[string]bool{}, map[string]bool{}
	for _, row := range listing.Rows {
		if !row.open() {
			continue
		}
		open[row.worker()] = true
		if row.needsYou() {
			needYou[row.worker()] = true
		}
	}
	summary.Open, summary.NeedYou = len(open), len(needYou)
	return summary, nil
}

// worker names the thread the row went to, with its computer, so requests
// to one thread count it once; a row whose thread is not made yet is its
// own.
func (row ThreadAgentRow) worker() string {
	if row.ThreadID == "" {
		return "request:" + row.Token
	}
	return row.ComputerID + "/" + row.ThreadID
}

// open reports whether the row is open: its request has not settled, or
// the thread its spawn made is not at rest.
func (row ThreadAgentRow) open() bool {
	if row.Worker != "" {
		return true
	}
	switch row.State {
	case threadtools.RequestUnconfirmed, threadtools.RequestAccepted, threadtools.RequestRunning, threadtools.RequestBlocked:
		return true
	default:
		return false
	}
}

// needsYou reports whether the row waits on the user, as its blocked
// fields say: blocked on a prompt no agent was given.
func (row ThreadAgentRow) needsYou() bool {
	return row.BlockedOn != "" && row.BlockedRoutedTo == ""
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

// threadAgentTreeDepth reads a listing through view nested at most maxDepth
// levels, and what stops each row: a paired computer listing for a
// caller's nested row has fewer left.
func (a *App) threadAgentTreeDepth(ctx context.Context, view threadAgentView, threadID string, maxDepth int, peers threadAgentPeerMode) (ThreadAgentRows, []threadAgentEntry, error) {
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
		// A remote spawn at the last level still reads its thread's state;
		// it nests none of its rows.
		if request.RemoteListingReach() {
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
		// At the last level every row is past the bound, which the spawn's
		// row says (NestedHidden).
		out.Hidden = out.Hidden || (remote[i].depth < maxDepth && answers[i].listing.Hidden)
	}
	if len(out.Rows) > threadAgentRowLimit {
		out.Rows, entries, out.Hidden = out.Rows[:threadAgentRowLimit], entries[:threadAgentRowLimit], true
	}
	return out, entries, nil
}

// remoteAgentAnswers reads each remote spawn's rows: the last answer, with
// another asked in the background, or every computer now, concurrently,
// recorded as any answer is and nested as the kept one, so a computer that
// does not answer now shows its last answer either way. A spawn whose
// computer is no longer paired is asked nothing and answers that it is not.
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
			// The levels its rows may still use, none at the bound: there the
			// answer is the thread's state alone, and that computer asks
			// none further on.
			depth := maxDepth - remote[i].depth
			answers[i] = a.readPeerAgentRows(call, remote[i].spawn, depth)
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
// reason on the spawn's row, and the state its last answer gave.
func (a *App) nestPeerAgentRows(parent *ThreadAgentRow, slot threadAgentRemoteSlot, answer threadAgentPeerAnswer, maxDepth int) ([]ThreadAgentRow, []threadAgentEntry) {
	// That computer says why the thread is not at rest, and whether it is
	// open; one with only open work below it there waits on it, as its
	// row's nested_open says once recorded. A failed call is no news of
	// rest: the last answer stands, which a refusal clears
	// (noteAnswerLocked). It says where the thread is placed and what it
	// waits on there too.
	switch open := answer.listing.Open; {
	case answer.listing.Worker != "":
		parent.Worker = answer.listing.Worker
	case open != nil && *open && parent.Worker == "":
		parent.Worker = workerWaiting
	}
	peerSpawnReport(parent, answer)
	if parent.Worker == workerBlocked || parent.State == threadtools.RequestBlocked {
		a.peerSpawnBlocked(parent, answer.listing)
	}
	if parent.open() && parent.BlockedOn == "" {
		parent.Activity = answer.listing.Activity
	}
	if answer.err != nil {
		parent.NestedUnavailable = threadAgentPeerUnavailable(parent.Computer, answer.err)
		return nil, nil
	}
	if parent.Provider == "" {
		parent.Provider = answer.listing.ThreadProvider
	}
	if slot.depth >= maxDepth && answer.listing.Hidden {
		// The thread's state alone, for a thread with rows past the bound.
		parent.NestedHidden = true
	}
	self, _ := a.backendIdentity()
	rows := make([]ThreadAgentRow, 0, len(answer.listing.Rows))
	entries := make([]threadAgentEntry, 0, len(answer.listing.Rows))
	for _, row := range answer.listing.Rows {
		row.Depth += slot.depth
		if row.Depth >= maxDepth {
			if row.Depth == slot.depth {
				parent.NestedHidden = true
			}
			for i := len(rows) - 1; i >= 0; i-- {
				if rows[i].Depth == row.Depth-1 {
					rows[i].NestedHidden = true
					break
				}
			}
			continue
		}
		row.OwnerComputerID = slot.spawn.TargetComputerID
		// The placement stays the one the computer that holds the thread
		// reported. A prompt routed back to that computer's spawner is
		// routed to a thread there, not here, and named with that computer.
		if row.BlockedRoutedHere {
			row.BlockedRoutedTo, row.BlockedRoutedHere = row.BlockedRoutedTo+" on "+parent.Computer, false
		}
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

// peerSpawnReport sets what a paired computer's spawn did and where its
// thread is placed there, as that computer's last answer reported it: the
// one projection of a paired spawn's counts, placement and archive state,
// for its open row and its Past row alike.
func peerSpawnReport(row *ThreadAgentRow, answer threadAgentPeerAnswer) {
	row.ToolCalls, row.Tokens = answer.listing.ToolCalls, answer.listing.Tokens
	row.RuntimeMs, row.TurnOpen, row.TurnStartedAt = answer.listing.RuntimeMs, answer.listing.TurnOpen, answer.listing.TurnStartedAt
	row.Placement, row.Archived = answer.listing.Placement, answer.listing.Archived
}

// peerSpawnBlocked sets what a blocked paired spawn's thread waits on, as
// that computer's answer reports it; a row the answer says nothing for
// keeps what the request's poll said. A prompt that computer routed back
// to this spawn's holder is named as this computer names the holder.
func (a *App) peerSpawnBlocked(row *ThreadAgentRow, listing ThreadAgentRows) {
	if listing.BlockedOn == "" {
		return
	}
	row.BlockedOn, row.BlockedDetail, row.BlockedRoutedTo = listing.BlockedOn, listing.BlockedDetail, listing.BlockedRoutedTo
	row.BlockedRoutedHere = listing.RoutedToAsker
	if listing.RoutedToAsker {
		row.BlockedRoutedTo = a.routedHereTitle(row.ComputerID, row.ThreadID, row.CallerThreadID, listing.BlockedRoutedTo)
	}
}

// routedHereTitle names the thread here that holds the prompt the paired
// computer computerID routed back here for its thread workerID: the thread
// that prompt's wake goes to, the caller now of the spawn from here that
// made the worker (store.RemoteSpawnLinkTo, store.RemotePromptHolder, as
// deliverRemotePrompt reads it), which a handoff moves. While no thread
// here can take it, which sends it back, it names named, the thread that
// computer reported, by its title here; and the name that computer gave
// when no title can be read, so the row still reads as routed rather than
// the person's.
func (a *App) routedHereTitle(computerID, workerID, named, reported string) string {
	spawn, found, err := a.store.RemoteSpawnLinkTo(computerID, workerID)
	if err == nil && found {
		var title string
		_, title, found, err = a.store.RemotePromptHolder(spawn.Token)
		if err == nil && found && title != "" {
			return title
		}
	}
	if err != nil {
		log.Printf("agent rows: read the thread here that holds the prompt of %s on %s: %v", workerID, computerID, err)
	}
	thread, err := a.store.GetThread(named)
	if err != nil {
		log.Printf("agent rows: read the title of %s, whose worker's prompt came back to it: %v", named, err)
		return cmp.Or(reported, named)
	}
	return cmp.Or(thread.Title, reported, named)
}

// pastPeerSpawnCounts sets the counts of the paired spawns on a page of
// Past rows from each one's last answer, and asks again in the background
// for any whose answer is missing or old, a page's worth at most; each
// answer that arrives frames its row. A spawn whose computer did not
// answer, or is no longer paired, says why (NestedUnavailable).
func (a *App) pastPeerSpawnCounts(rows []ThreadAgentRow, requests []store.ThreadAgentRequest) {
	for i := range rows {
		spawn := requests[i].ThreadRequest
		if spawn.Kind != store.ThreadRequestSpawn || spawn.TargetComputerID == "" || spawn.TargetThreadID == "" {
			continue
		}
		paired, err := a.peerPaired(spawn.TargetComputerID)
		if err == nil && !paired {
			err = errThreadAgentPeerUnpaired
		}
		if err != nil {
			rows[i].NestedUnavailable = threadAgentPeerUnavailable(rows[i].Computer, err)
			continue
		}
		answer, ok := a.cachedPeerAgentRows(spawn)
		if ok {
			peerSpawnReport(&rows[i], answer)
			if answer.err != nil {
				rows[i].NestedUnavailable = threadAgentPeerUnavailable(rows[i].Computer, answer.err)
			}
		}
		a.refreshPairedPeerAgentRows(spawn, threadAgentPeerRefreshEvery)
	}
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

// threadAgentView renders request rows for one read: the parked waits, the
// live workers and the settled window are read once for the whole listing.
// A view for
// counts renders each row's state and what it is blocked on, and reads
// none of the text a row shows: no answer head, no target names, no last
// messages.
type threadAgentView struct {
	app     *App
	waiting []string
	live    []string
	since   int64
	counts  bool
}

func newThreadAgentView(a *App) threadAgentView {
	return threadAgentView{
		app: a, waiting: a.parkedRequestTokens(), live: a.liveWorkerThreads(),
		since: time.Now().Add(-threadAgentSettledWindow).UnixMilli(),
	}
}

// tree reads root's requests nested at most depth levels, or only those
// of them tokens names.
func (v threadAgentView) tree(root string, depth int, tokens []string, limit int) ([]store.ThreadAgentRequest, bool, error) {
	head := threadAgentAnswerHead
	if v.counts {
		head = 0
	}
	return v.app.store.ListThreadAgentTree(store.ThreadAgentTreeRead{
		Root: root, Waiting: v.waiting, Live: v.live, SettledSince: v.since, Depth: depth,
		Limit: limit, AnswerHead: head, Tokens: tokens,
	})
}

// rows renders requests as their rows, with the threads they went to read
// once for them all.
func (v threadAgentView) rows(requests []store.ThreadAgentRequest) ([]ThreadAgentRow, error) {
	if v.counts {
		out := make([]ThreadAgentRow, 0, len(requests))
		for _, request := range requests {
			out = append(out, v.row(request, nil))
		}
		return out, nil
	}
	ids := make([]string, 0, len(requests))
	for _, request := range requests {
		if request.TargetComputerID == "" && request.TargetThreadID != "" {
			ids = append(ids, request.TargetThreadID)
			if request.OriginThreadID != "" {
				ids = append(ids, request.OriginThreadID)
			}
		}
	}
	names, err := v.app.store.ThreadNames(ids, time.Now())
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
	// A spawn's worker is read whatever its request's state: one that
	// settled is open while the thread it made is not at rest, by its own
	// state or, waiting on them, by its workers'. A remote spawn's own
	// state is its computer's answer (nestPeerAgentRows).
	var rest workerRest
	if local && row.Kind == store.ThreadRequestSpawn {
		rest = v.app.liveWorkerUnrest(row)
	}
	if rest.unrest == "" && request.TargetOpen {
		rest.unrest = workerWaiting
	}
	if rest.unrest == "" && local && !settled && v.app.restHeld(row.TargetThreadID, row.Token) {
		// A request held for its thread's rest, a send to the caller's own
		// worker among them, reads as that thread waits.
		rest = v.app.liveThreadRest(row.TargetThreadID)
		if rest.unrest == "" {
			rest.unrest = workerWaiting
		}
	}
	if rest.unrest == "" && !local && !settled && row.TargetThreadID != "" {
		// So does one a paired computer holds for its thread's rest, as
		// that computer last reported it.
		if line := v.app.remoteRequestBlocked(row.Token).waitingOn; line != "" {
			rest = workerRest{unrest: workerWaiting, detail: line}
		}
	}
	worker := rest.unrest
	live := !settled || worker != ""
	blocked := false
	var remote remoteBlocked
	var waiting threadWaiting
	switch {
	case !live || row.TargetThreadID == "":
	case !local:
		remote = v.app.remoteRequestBlocked(row.Token)
		blocked = remote.blocked
	default:
		waiting = v.app.threadWaitingOn(row.TargetThreadID)
		blocked = waiting.live.Blocked()
	}
	state := requestStateFrom(row, request.AnswerHead, request.HasLateReply, requestTitleFrom(row, names), blocked)
	out := ThreadAgentRow{
		Token: row.Token, CallerThreadID: row.CallerThreadID, Kind: row.Kind,
		ThreadID: row.TargetThreadID, OpenThreadID: row.TargetThreadID,
		ComputerID: row.TargetComputerID, Title: state.Title, State: state.State, Worker: worker,
		LateReply: request.HasLateReply, CreatedAt: row.CreatedAt, SettledAt: row.SettledAt,
		Depth: request.Depth, NestedHidden: request.NestedHidden,
		RestUnreported: row.RestUnreported, LateReplyExpiredAt: state.LateReplyExpiredAt,
	}
	if row.Kind == store.ThreadRequestAsk {
		// The copy that answers an ask is the worker: the row opens it while
		// it runs. Once the ask settles the copy is deleted, and the row
		// names the thread asked as that, never as its worker.
		out.AskedThreadID = row.OriginThreadID
		if _, there := names[row.TargetThreadID]; settled || (local && !there && !v.counts) {
			out.OpenThreadID = ""
		}
	}
	if row.TargetComputerID != "" {
		out.Computer, _ = v.app.threadTools().requestComputerName(row)
		out.Provider = row.TargetProvider
	}
	if settled {
		out.Answer = clipLine(state.Answer)
		out.AnswerKind = state.AnswerKind
		out.WakeItemID = request.WakeItemID
	}
	if name, ok := names[row.TargetThreadID]; ok && local {
		out.Provider = name.Provider
		if row.Kind == store.ThreadRequestSpawn {
			out.Placement, out.Archived = name.Placement, name.Archived
			// What the thread it made did, read with its name.
			tally := name.Tally
			out.ToolCalls, out.Tokens, out.RuntimeMs, out.TurnOpen = tally.ToolCalls, tally.Tokens, tally.RuntimeMs, tally.Running
			if tally.Running {
				out.TurnStartedAt = tally.TurnStartedAt
			}
		}
	}
	if asked, ok := names[row.OriginThreadID]; ok && local && row.Kind == store.ThreadRequestAsk {
		// An ask's row is the thread asked, while its copy runs and once
		// the copy is deleted; its kind says it was an ask.
		out.Title, out.Provider = asked.Title, asked.Provider
	}
	switch {
	case !live:
	case row.TargetComputerID != "":
		// That computer reads its target's prompts; it says whether one is
		// routed to a thread there rather than the person, or back to the
		// spawner here.
		if state.State == threadtools.RequestBlocked {
			out.BlockedOn, out.BlockedRoutedTo = "person", remote.routedTo
			if remote.routedHere() {
				// That computer names the thread the link came from: this
				// row's caller, or another thread here, the spawner of the
				// thread this row's request went to.
				named := remote.routedToThread
				if remote.routedToCaller {
					named = row.CallerThreadID
				}
				out.BlockedRoutedTo = v.app.routedHereTitle(row.TargetComputerID, row.TargetThreadID, named, remote.routedTo)
				out.BlockedRoutedHere = true
			}
		}
		out.Activity = rest.detail
	case row.TargetThreadID != "":
		out.BlockedOn, out.BlockedDetail, out.BlockedRoutedTo = waiting.on, waiting.detail, waiting.routedTo()
		out.Activity = v.app.threadAgentActivity(row.TargetThreadID)
		if out.Activity == "" {
			// A waiting worker's line is what it waits on.
			out.Activity = rest.detail
		}
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
// first approval, else its first question, with the route that prompt took
// (nil while the person owns it).
func threadAgentBlockedOn(snapshot triage.LiveStateSnapshot) (on, detail string, route *provider.PromptRoute) {
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
		return "approval", clipLine(detail), request.RoutedTo
	}
	if questions := snapshot.Interactive.UserInputs; len(questions) > 0 {
		request := questions[0]
		detail = request.Title
		if len(request.Questions) > 0 {
			detail = firstNonEmpty(request.Questions[0].Question, request.Questions[0].Header, detail)
		}
		return "question", clipLine(detail), request.RoutedTo
	}
	return "", "", nil
}

// withholdApprovalDetail clears what an approval asks from the listing's
// own prompt and from each of its rows, nested and a paired computer's
// included, for a session without approvals:respond. That detail quotes
// the command, which only that scope is shown, as provider:approval
// carries it (transport/event_channels.go); the session still reads that
// the worker waits on an approval and where it went. A question's detail
// is thread content, which threads:read reads. A call with no session, an
// agent's own thread tools on this computer, keeps it. Every reader of the
// listing that answers a session passes its answer here.
func (a *App) withholdApprovalDetail(ctx context.Context, listing *ThreadAgentRows) {
	if a.requireScope(ctx, transport.ScopeApprovalsRespond, "") == nil {
		return
	}
	if listing.BlockedOn == "approval" {
		listing.BlockedDetail = ""
	}
	for i := range listing.Rows {
		if listing.Rows[i].BlockedOn == "approval" {
			listing.Rows[i].BlockedDetail = ""
		}
	}
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

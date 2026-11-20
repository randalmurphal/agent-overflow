package app

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
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

// threadAgentKindNudge is the Kind of the row that records a nudge's run
// (store.StartNudgeRun): work the spawner did not ask for, with no answer.
const threadAgentKindNudge = "nudge"

// ThreadAgentRow is one request a thread made of another thread, with that
// thread as it stands.
type ThreadAgentRow struct {
	Token string `json:"token"`
	// CallerThreadID made the request: the listed thread for a depth-0 row,
	// the thread a nested row's parent spawned otherwise.
	CallerThreadID string `json:"callerThreadId"`
	// Kind is spawn, send or ask, or nudge for the run of an owned worker
	// the person's or another thread's message started (threadAgentKindNudge).
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
	// paired computer for one there, nested rows included. A send to a
	// thread its caller spawned, which counts a run of that worker, carries
	// them too. Empty and false for every other row and from a computer that
	// does not report them.
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
	// Worker is why the thread a spawn made, or a send's run is on, is not
	// at rest while the row's run is open, whatever State says: blocked,
	// running, queued, or waiting on what it started (background work, a
	// loop, watch or reminder, a provider timer) or on its own workers,
	// open work at any depth below it; empty while it is at rest, once the
	// run ended, and for every other row. A row whose worker is not at
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
	// RunEnd is the listed thread's figures over its life when the asking
	// thread's last run on it ended, answered by agent_rows when that
	// thread's requests there track runs and none is open
	// (store.ReceiptRunOf). The asking computer ends its runs on the thread
	// with them, whoever runs the thread since (endRemoteRunsAtRest). Nil
	// while a run is open, and from a computer that predates it.
	RunEnd *ThreadAgentRunEnd `json:"runEnd,omitempty"`
	// RunOpen says a run of the asking thread's on the listed thread is open
	// there, its requests' or a nudge's: the asking computer then ends none
	// of its runs on the thread for Open alone (endRemoteRunsAtRest). False
	// from a computer that predates it, which reads as before.
	RunOpen bool `json:"runOpen,omitempty"`
	// Nudges is the nudge runs of the listed thread's link to the asking
	// spawn that the asking computer has not taken (store.ReceiptRunOf):
	// the open one first, then the ended ones oldest first, a bounded
	// number. The asking computer records each as its nudge row
	// (store.RecordRemoteNudgeRun) and names the ended ones it recorded in
	// its next read (threadAgentPeerArgs.NudgesTaken), after which they are
	// not reported again. Empty when there is none, and from a computer
	// that predates it, whose nudges show no row.
	Nudges []ThreadAgentNudge `json:"nudges,omitempty"`
}

// ThreadAgentNudge is a nudge's run on a paired computer's worker: its
// token, when it began on that computer's clock, what the worker had done
// over its life then, and once it ended, what it had done then.
type ThreadAgentNudge struct {
	Token     string             `json:"token"`
	StartedAt int64              `json:"startedAt"`
	Base      ThreadAgentRunEnd  `json:"base"`
	End       *ThreadAgentRunEnd `json:"end,omitempty"`
}

// ThreadAgentRunEnd is what a thread had done over its life when a run on it
// ended.
type ThreadAgentRunEnd struct {
	ToolCalls int64 `json:"toolCalls"`
	Tokens    int64 `json:"tokens"`
	RuntimeMs int64 `json:"runtimeMs"`
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
		// A row in a spawn's place on a paired computer nests that spawn's
		// thread there, whichever of its caller's requests the row shows.
		spawn := *request
		if node := requests[index].Node; node != "" && node != request.Token && request.TargetComputerID != "" {
			row, found, err := a.store.GetThreadRequest(node)
			if err != nil {
				return ThreadAgentRows{}, nil, err
			}
			if found {
				spawn = row
			}
		}
		// A remote spawn at the last level still reads its thread's state;
		// it nests none of its rows.
		if spawn.RemoteListingReach() {
			remote = append(remote, threadAgentRemoteSlot{index: index, depth: depth + 1, spawn: spawn, shown: *request, ended: remoteSpawnEnded(spawn, *request)})
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

// remoteSpawnEnded reports whether a paired computer's spawn no longer
// carries its worker into the listing, as the store reads a local spawn's
// link (spawnActiveSQL): its own run ended, and the request its row shows
// is no send of its caller's whose run is open there.
func remoteSpawnEnded(spawn, shown store.ThreadRequest) bool {
	return spawn.RunEnded() && !(shown.Kind == store.ThreadRequestSend && shown.RunOpen())
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

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The Agents rows across computers: a thread spawned on a paired computer
// lists and stops as one spawned here does. Two forwarded tools ride the
// existing peer calls (ThreadToolQuery and ThreadToolCall):
//
//   - agent_rows lists the Agents rows of a thread the caller's thread
//     spawned there, nested to the depth left, so the caller's pane nests
//     them under its own row.
//   - agent_stop runs a Stop's cascade there: for a whole spawned thread,
//     its Backgrounded items and its own Agents rows; for one nested row by
//     token, that row's Stop.
//
// Each is refused unless the thread is one the calling device's thread
// spawned there, or one reached from it through that computer's own spawn
// lineage. A computer that predates them answers that they are not thread
// tools calls; the caller then shows and stops the request and the turn
// alone, and says so. An agent_rows call names the spawn it asks about, so
// the answer also carries the prompts that computer routed to the spawn's
// thread here (app_worker_prompts_peer.go).

// Forwarded tool names of the Agents rows.
const (
	threadAgentPeerRows = "agent_rows"
	threadAgentPeerStop = "agent_stop"
)

// threadAgentPeerListTimeout bounds a forwarded listing: a pane's read must
// not wait long on a computer that does not answer. A forwarded Stop runs
// interrupts there, each bounded by its provider's own interrupt timeout, so
// it waits longer. Both wait less than the destination's own budget
// (threadPeerCallWait) on purpose: a listing lost to the bound is read again
// on the pane's next refresh, and a Stop the destination finishes later
// settles its receipts there, which the source's poll collects.
const (
	threadAgentPeerListTimeout = 5 * time.Second
	threadAgentPeerStopTimeout = 45 * time.Second
	// threadAgentPeerServeTimeout bounds the listing an agent_rows answer
	// reads from a computer further on: the computer that asked gives the
	// whole answer threadAgentPeerListTimeout, and a computer that does not
	// answer there must not use it all up.
	threadAgentPeerServeTimeout = threadAgentPeerListTimeout / 2
)

// threadAgentPeerArgs are the forwarded Agents tools' arguments.
type threadAgentPeerArgs struct {
	ThreadID string `json:"thread_id"`
	// Token names one row of ThreadID to stop; empty stops the thread's
	// whole work.
	Token string `json:"token,omitempty"`
	// Depth is how many nesting levels the listing may still use.
	Depth int `json:"depth,omitempty"`
	// StateOnly asks for the thread's state without its rows: the caller
	// lists the spawn at its last level. It is sent with Depth 1, which a
	// computer that predates it lists instead.
	StateOnly bool `json:"state_only,omitempty"`
	// Spawn is the caller's spawn token, the token of the receipt whose
	// link owns ThreadID there: an agent_rows answer then lists the prompts
	// routed to the caller's thread through that link (Prompts).
	Spawn string `json:"spawn,omitempty"`
	// Held names the listed prompts the caller holds now: their leases
	// are renewed.
	Held []string `json:"held,omitempty"`
}

// errThreadAgentPeerTimedOut marks a forwarded call that ran out its own
// time: the computer did not answer, as against refusing or being
// unreachable at once.
var errThreadAgentPeerTimedOut = errors.New("the call timed out")

// errThreadAgentPeerUnpaired marks a spawn whose computer is no longer
// paired: nothing is asked of it.
var errThreadAgentPeerUnpaired = errors.New("that computer is no longer paired")

// errThreadAgentPeerOlder marks a paired computer that predates the
// forwarded Agents tools.
var errThreadAgentPeerOlder = errors.New("that computer runs an older version: its background work and agents are not shown or stopped from here")

// threadAgentPeerOlder reports a refusal that is an older computer not
// knowing the tool: it names the tool as not a thread tools call.
func threadAgentPeerOlder(err error) bool {
	var remote *rpcclient.Error
	if errors.As(err, &remote) {
		return remote.Code == threadtools.CodeInvalidRequest && strings.Contains(remote.Message, " is not a thread tools ")
	}
	if code, text, ok := errorsx.PublicDetails(err); ok {
		return code == threadtools.CodeInvalidRequest && strings.Contains(text, " is not a thread tools ")
	}
	return false
}

// callThreadAgentPeer forwards one Agents tool to a paired computer.
func (a *App) callThreadAgentPeer(ctx context.Context, method, computerID, sourceThreadID string, args threadAgentPeerArgs, out any) (attachedbackends.PeerRead, error) {
	if a.backends == nil {
		return attachedbackends.PeerRead{}, errors.New("no paired computers are reachable")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return attachedbackends.PeerRead{}, fmt.Errorf("encode %s: %w", method, err)
	}
	tool, timeout := threadAgentPeerRows, threadAgentPeerListTimeout
	if method == "ThreadToolCall" {
		tool, timeout = threadAgentPeerStop, threadAgentPeerStopTimeout
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reply ThreadPeerReply
	origin, err := a.backends.ReadThreadPeer(call, computerID, transport.CapabilityThreadTools, "", method, &reply, ThreadPeerCall{
		Tool: tool, Args: encoded, Token: newThreadRequestToken(), Source: a.threadAgentCaller(sourceThreadID),
	})
	if err != nil {
		if threadAgentPeerOlder(err) {
			return origin, errThreadAgentPeerOlder
		}
		err = a.threadOperationError(tool, computerID, args.ThreadID, err)
		if errors.Is(call.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return origin, fmt.Errorf("%w: %w", errThreadAgentPeerTimedOut, err)
		}
		return origin, err
	}
	if err := json.Unmarshal(reply.Result, out); err != nil {
		return origin, fmt.Errorf("read %s from %s: %w", tool, computerID, err)
	}
	return origin, nil
}

// peerThreadAgentRows reads the Agents rows of the thread spawn made on a
// paired computer, nested depth levels, or with depth 0 the thread's state
// alone, and the prompts its worker routed here, renewing those held here.
func (a *App) peerThreadAgentRows(ctx context.Context, spawn store.ThreadRequest, depth int) (ThreadAgentRows, attachedbackends.PeerRead, error) {
	args := threadAgentPeerArgs{ThreadID: spawn.TargetThreadID, Depth: depth, Spawn: spawn.Token, Held: a.heldRemotePrompts(spawn)}
	if depth <= 0 {
		args.Depth, args.StateOnly = 1, true
	}
	var rows ThreadAgentRows
	origin, err := a.callThreadAgentPeer(ctx, "ThreadToolQuery", spawn.TargetComputerID, spawn.MadeIn(), args, &rows)
	return rows, origin, err
}

// peerThreadAgentStop runs a Stop's cascade on a paired computer.
func (a *App) peerThreadAgentStop(ctx context.Context, computerID, sourceThreadID, threadID, token string) (ThreadAgentStop, error) {
	var stop ThreadAgentStop
	_, err := a.callThreadAgentPeer(ctx, "ThreadToolCall", computerID, sourceThreadID,
		threadAgentPeerArgs{ThreadID: threadID, Token: token}, &stop)
	return stop, err
}

// serveThreadAgentPeer answers a forwarded Agents tool.
func (a *App) serveThreadAgentPeer(ctx context.Context, owner string, call ThreadPeerCall) (ThreadPeerReply, error) {
	source := strings.TrimSpace(call.Source.ThreadID)
	var args threadAgentPeerArgs
	if err := json.Unmarshal(call.Args, &args); err != nil || !entityid.Valid(source) || !entityid.Valid(args.ThreadID) {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"An agents call must name the thread it came from and the thread it is about.", err)
	}
	reached, err := a.peerReachesThread(owner, source, args.ThreadID)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	if !reached {
		return ThreadPeerReply{}, threadNotYoursToInterrupt(args.ThreadID)
	}
	if hook := a.peerAgentsReachedForTest; hook != nil {
		hook(args.ThreadID)
	}
	var result any
	switch call.Tool {
	case threadAgentPeerRows:
		listing, err := a.answerThreadAgentRows(ctx, owner, source, args)
		if err != nil {
			return ThreadPeerReply{}, a.peerAnswerError(owner, source, args.ThreadID, err)
		}
		result = listing
	case threadAgentPeerStop:
		stop, err := a.stopThreadAgentWorkHere(ctx, args.ThreadID, args.Token)
		if err != nil {
			return ThreadPeerReply{}, a.peerAnswerError(owner, source, args.ThreadID, err)
		}
		result = stop
	default:
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("%s is not a thread tools call.", call.Tool), nil)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ThreadPeerReply{}, fmt.Errorf("thread tools: encode %s result: %w", call.Tool, err)
	}
	return ThreadPeerReply{Result: encoded}, nil
}

// peerAnswerError is what an agents call that passed its reach check and
// then failed answers: the refusal when the thread stopped being the
// caller's meanwhile, deleted or moved away, which the caller takes as
// certain, and err otherwise.
func (a *App) peerAnswerError(owner, source, threadID string, err error) error {
	reached, reachErr := a.peerReachesThread(owner, source, threadID)
	switch {
	case reachErr != nil:
		return fmt.Errorf("%w (and read whether the thread is still the caller's: %v)", err, reachErr)
	case !reached:
		return threadNotYoursToInterrupt(threadID)
	}
	return err
}

// answerThreadAgentRows is agent_rows on this computer: the thread's rows
// nested the levels asked, none for a call that asks for its state alone,
// and what the asking computer cannot read from rows that have no room for
// it, or from none. No spawn here made the thread, so no row here follows
// it: the answer says why it is not at rest (Worker), and whether it is
// open, at rest or not, with open work at any depth below it, as a spawn
// of it here would read it (store.ReadThreadAgentState). That is read
// after the rows, so the answers of computers further on that the rows
// asked for are recorded in it. A call for the state alone asks no other
// computer: Hidden says whether the thread has rows. It says where the
// thread is placed here, whether it is archived, and what prompt it waits
// on, routed back to the calling thread or not (RoutedToAsker),
// and for a call that names the spawn whose link owns it, the prompts
// routed to that spawn's thread (peerPromptsOf). Once the calling thread's
// runs on it all ended here, it says what the thread had done when the last
// ended (RunEnd).
func (a *App) answerThreadAgentRows(ctx context.Context, owner, source string, args threadAgentPeerArgs) (ThreadAgentRows, error) {
	var listing ThreadAgentRows
	if !args.StateOnly {
		depth := args.Depth
		if depth <= 0 || depth > threadAgentRowDepth {
			depth = threadAgentRowDepth
		}
		var err error
		listing, _, err = a.threadAgentTreeDepth(ctx, newThreadAgentView(a), args.ThreadID, depth, threadAgentPeersLive)
		if err != nil {
			return ThreadAgentRows{}, err
		}
	}
	thread, err := a.store.GetThread(args.ThreadID)
	if err != nil {
		return ThreadAgentRows{}, fmt.Errorf("read thread %s: %w", args.ThreadID, err)
	}
	listing.Title = thread.Title
	listing.ThreadProvider = thread.Provider
	listing.Placement, listing.Archived = thread.Placement, thread.Archived
	waiting := a.threadWaitingOn(args.ThreadID)
	listing.BlockedOn, listing.BlockedDetail, listing.BlockedRoutedTo = waiting.on, waiting.detail, waiting.routedTo()
	holder, err := a.routedToAsker(args.ThreadID, waiting.route, owner)
	if err != nil {
		return ThreadAgentRows{}, err
	}
	listing.RoutedToAsker = holder != "" && holder == source
	if listing.Prompts, err = a.peerPromptsOf(ctx, owner, source, thread.Title, args); err != nil {
		return ThreadAgentRows{}, err
	}
	rest, err := a.threadRest(args.ThreadID)
	if err != nil {
		return ThreadAgentRows{}, fmt.Errorf("read whether thread %s is at rest: %w", args.ThreadID, err)
	}
	if !rest.below {
		listing.Worker = rest.unrest
	}
	open := rest.unrest != ""
	listing.Open = &open
	run, err := a.store.ReceiptRunOf(owner, source, args.ThreadID)
	if err != nil {
		return ThreadAgentRows{}, err
	}
	if run.End != nil {
		listing.RunEnd = runEndOf(*run.End)
	}
	listing.RunOpen = run.Open
	if nudge := run.Nudge; nudge != nil {
		listing.Nudge = &ThreadAgentNudge{Token: nudge.Token, StartedAt: nudge.StartedAt, Base: *runEndOf(nudge.Base)}
		if nudge.End != nil {
			listing.Nudge.End = runEndOf(*nudge.End)
		} else {
			listing.RunOpen = true
		}
	}
	tallies, err := a.store.ThreadTallies([]string{args.ThreadID}, time.Now())
	if err != nil {
		return ThreadAgentRows{}, fmt.Errorf("read what thread %s did: %w", args.ThreadID, err)
	}
	// As this computer's own row of it reads (threadAgentView.row).
	tally := tallies[args.ThreadID]
	listing.ToolCalls, listing.Tokens = tally.ToolCalls, tally.Tokens
	listing.RuntimeMs, listing.TurnOpen = tally.RuntimeMs, tally.Running
	if tally.Running {
		listing.TurnStartedAt = tally.TurnStartedAt
	}
	if open {
		// As this computer's own row of it reads (threadAgentView.row).
		listing.Activity = a.threadAgentActivity(args.ThreadID)
		if listing.Activity == "" {
			listing.Activity = rest.detail
		}
	}
	if args.StateOnly {
		state, err := a.threadAgentState(args.ThreadID)
		if err != nil {
			return ThreadAgentRows{}, err
		}
		listing.Hidden = state.Rows
	}
	a.withholdApprovalDetail(ctx, &listing)
	return listing, nil
}

// runEndOf is figures as the agent_rows answer carries them.
func runEndOf(figures store.RunFigures) *ThreadAgentRunEnd {
	return &ThreadAgentRunEnd{ToolCalls: figures.ToolCalls, Tokens: figures.Tokens, RuntimeMs: figures.TurnMs}
}

// stopThreadAgentWorkHere is agent_stop on this computer: one row of
// threadID's listing by token, or the whole work of threadID, reported
// stopped when the thread was not at rest or the rest stopped anything of it
// or below it (restStoppedWork), and ended otherwise.
func (a *App) stopThreadAgentWorkHere(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	if token != "" {
		return a.stopThreadAgentRowIn(ctx, threadID, token)
	}
	stop := ThreadAgentStop{Outcome: threadAgentStopped}
	working := a.workerNotAtRest(threadID, &stop)
	a.stopThreadAgentTarget(ctx, threadID, map[string]bool{threadID: true}, &stop)
	if !working && !restStoppedWork(ThreadAgentStop{}, stop) {
		stop.Outcome = threadAgentEnded
	}
	return stop, nil
}

// peerReachesThread reports whether threadID is a thread the source thread
// of this device spawned here, or one reached from such a thread through
// this computer's own spawn lineage, within the nesting bound. The spawn's
// receipt is kept as its lineage while the thread exists, past the
// retention floor. A false answer is certain, as the caller records it
// (recordPeerOpenWork): a failed read is an error, not a refusal.
func (a *App) peerReachesThread(owner, sourceThreadID, threadID string) (bool, error) {
	current := threadID
	for range threadAgentRowDepth + 1 {
		spawned, err := a.store.ThreadSpawnedForSource(current, owner, sourceThreadID)
		if err != nil || spawned {
			return spawned, err
		}
		thread, err := a.store.GetThread(current)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if thread.SpawnedByThreadID == "" {
			return false, nil
		}
		current = thread.SpawnedByThreadID
	}
	return false, nil
}

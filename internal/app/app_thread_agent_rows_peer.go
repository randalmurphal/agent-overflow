package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
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
// alone, and says so.

// Forwarded tool names of the Agents rows.
const (
	threadAgentPeerRows = "agent_rows"
	threadAgentPeerStop = "agent_stop"
)

// threadAgentPeerListTimeout bounds a forwarded listing: a pane's read must
// not wait long on a computer that does not answer. A forwarded Stop runs
// interrupts there, each bounded by its provider's own interrupt timeout, so
// it waits longer.
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
func (a *App) callThreadAgentPeer(ctx context.Context, method, computerID, sourceThreadID string, args threadAgentPeerArgs, out any) error {
	if a.backends == nil {
		return errors.New("no paired computers are reachable")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("encode %s: %w", method, err)
	}
	tool, timeout := threadAgentPeerRows, threadAgentPeerListTimeout
	if method == "ThreadToolCall" {
		tool, timeout = threadAgentPeerStop, threadAgentPeerStopTimeout
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reply ThreadPeerReply
	if err := a.backends.CallThreadPeer(call, computerID, method, &reply, ThreadPeerCall{
		Tool: tool, Args: encoded, Token: newThreadRequestToken(), Source: a.threadAgentCaller(sourceThreadID),
	}); err != nil {
		if threadAgentPeerOlder(err) {
			return errThreadAgentPeerOlder
		}
		err = a.threadOperationError(tool, computerID, args.ThreadID, err)
		if errors.Is(call.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("%w: %w", errThreadAgentPeerTimedOut, err)
		}
		return err
	}
	if err := json.Unmarshal(reply.Result, out); err != nil {
		return fmt.Errorf("read %s from %s: %w", tool, computerID, err)
	}
	return nil
}

// peerThreadAgentRows reads the Agents rows of a thread this computer's
// thread spawned on a paired computer.
func (a *App) peerThreadAgentRows(ctx context.Context, computerID, sourceThreadID, threadID string, depth int) (ThreadAgentRows, error) {
	var rows ThreadAgentRows
	err := a.callThreadAgentPeer(ctx, "ThreadToolQuery", computerID, sourceThreadID,
		threadAgentPeerArgs{ThreadID: threadID, Depth: depth}, &rows)
	return rows, err
}

// peerThreadAgentStop runs a Stop's cascade on a paired computer.
func (a *App) peerThreadAgentStop(ctx context.Context, computerID, sourceThreadID, threadID, token string) (ThreadAgentStop, error) {
	var stop ThreadAgentStop
	err := a.callThreadAgentPeer(ctx, "ThreadToolCall", computerID, sourceThreadID,
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
	var result any
	switch call.Tool {
	case threadAgentPeerRows:
		depth := args.Depth
		if depth <= 0 || depth > threadAgentRowDepth {
			depth = threadAgentRowDepth
		}
		listing, _, err := a.threadAgentTreeDepth(ctx, args.ThreadID, depth, threadAgentPeersLive)
		if err != nil {
			return ThreadPeerReply{}, err
		}
		thread, err := a.store.GetThread(args.ThreadID)
		if err != nil {
			return ThreadPeerReply{}, fmt.Errorf("read thread %s: %w", args.ThreadID, err)
		}
		listing.ThreadProvider = thread.Provider
		result = listing
	case threadAgentPeerStop:
		stop, err := a.stopThreadAgentWorkHere(ctx, args.ThreadID, args.Token)
		if err != nil {
			return ThreadPeerReply{}, err
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

// stopThreadAgentWorkHere is agent_stop on this computer: one row of
// threadID's listing by token, or the whole work of threadID.
func (a *App) stopThreadAgentWorkHere(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	if token != "" {
		return a.stopThreadAgentRowIn(ctx, threadID, token)
	}
	stop := ThreadAgentStop{Outcome: threadAgentStopped}
	a.stopThreadAgentTarget(ctx, threadID, map[string]bool{threadID: true}, &stop)
	return stop, nil
}

// peerReachesThread reports whether threadID is a thread the source thread
// of this device spawned here, or one reached from such a thread through
// this computer's own spawn lineage, within the nesting bound.
func (a *App) peerReachesThread(owner, sourceThreadID, threadID string) (bool, error) {
	current := threadID
	for range threadAgentRowDepth + 1 {
		receipts, err := a.store.ListThreadRequestReceiptsForThread(current)
		if err != nil {
			return false, err
		}
		for _, receipt := range receipts {
			if receipt.Kind == store.ThreadRequestSpawn && receipt.OwnerDeviceID == owner && receipt.SourceThreadID == sourceThreadID {
				return true, nil
			}
		}
		thread, err := a.store.GetThread(current)
		if err != nil || thread.SpawnedByThreadID == "" {
			return false, nil
		}
		current = thread.SpawnedByThreadID
	}
	return false, nil
}

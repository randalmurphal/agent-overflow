package app

import (
	"context"
	"encoding/json"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// ThreadToolWatch runs one thread_status wait on thread ids forwarded from a
// paired computer, and ends with its caller: a wait the calling computer
// gave up on stops reading this computer's live state at once rather than
// at the end of its slice. It is ThreadToolQuery's thread_status narrowed to
// the branch that reads threads, which reaches only store reads, live state
// and an in-memory wait registration, so it can be caller-bound; every
// other read stays on ThreadToolQuery. A computer on v0.0.19 does not serve
// it, and the caller asks that one through ThreadToolQuery.
//
//ao:scope threads:read
//ao:route selected
//ao:budget 1m
//ao:callerbound
func (a *App) ThreadToolWatch(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if _, err := a.remoteCommandOwner(ctx); err != nil {
		return ThreadPeerReply{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "watch a thread"); err != nil {
		return ThreadPeerReply{}, err
	}
	var args struct {
		ThreadIDs []string `json:"thread_ids"`
	}
	if call.Tool != "thread_status" || json.Unmarshal(call.Args, &args) != nil || len(args.ThreadIDs) == 0 {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A forwarded watch is a thread_status wait on thread_ids.", nil)
	}
	return a.runThreadPeerTool(ctx, call)
}

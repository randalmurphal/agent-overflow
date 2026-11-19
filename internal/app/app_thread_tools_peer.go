package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The destination half of cross-computer thread tools: the five bound
// methods a paired computer reaches. The shapes they carry are in
// app_thread_tools_peer_wire.go, and a forwarded request's half in
// app_thread_tools_peer_requests.go.
//
// They serve whatever this computer's own thread tools switch says. The
// switch means "agents on this computer get the tools"; pairing already
// says which computers belong to the user, and a paired computer's agents
// reach this one either way (docs/specs/agent-thread-tools.md,
// "Availability and permissions").
//
// Authorization is the authenticated device of the calling computer, the
// same principal remote jobs use, plus the method's own scope. A receipt
// records that device, so a second device of the same computer cannot
// redeem another device's token.
//
// A forwarded tool call runs through threadtools.CallForwarded rather than
// Call. A forwarded call runs on this computer alone: it sees no paired
// computers of its own, so a fan-out, a resolution or a search never crosses
// back to the computer that asked, and an export writes a file this computer
// can serve in chunks rather than a path the reader cannot open.

// ThreadToolResolve answers one thread reference against this computer's
// threads. It is typed rather than a tool call so the caller can compare
// an ambiguity across computers without decoding rendered text.
//
//ao:scope threads:read
//ao:route selected
//ao:callerbound
func (a *App) ThreadToolResolve(ctx context.Context, resolve ThreadPeerResolve) (threadtools.Resolution, error) {
	if _, err := a.remoteCommandOwner(ctx); err != nil {
		return threadtools.Resolution{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "resolve a thread"); err != nil {
		return threadtools.Resolution{}, err
	}
	caller, err := a.threadPeerCaller(ThreadPeerCall{Source: resolve.Source})
	if err != nil {
		return threadtools.Resolution{}, err
	}
	// Forwarded: a refusal names this computer to the caller on its own.
	return a.threadToolsAdapter().ResolveThreadRef(threadtools.WithForwarded(threadtools.WithCaller(ctx, caller)), resolve.Ref)
}

// ThreadToolQuery runs one read tool here and returns its JSON result
// unchanged. The caller stamps its own view of this computer onto the
// rows, which is what keeps grouping correct when this computer has no
// pairings of its own.
//
//ao:scope threads:read
//ao:route selected
//ao:budget 1m
func (a *App) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "read a thread"); err != nil {
		return ThreadPeerReply{}, err
	}
	switch call.Tool {
	case threadAgentPeerRows:
		return a.serveThreadAgentPeer(ctx, owner, call)
	case "thread_search", "thread_show", "thread_item", "thread_options", "thread_status":
	default:
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("%s is not a thread tools read call.", call.Tool), nil)
	}
	return a.runThreadPeerTool(ctx, call)
}

// ThreadToolCall runs one write tool here. spawn, send, ask and cancel
// carry the source's token and mint or settle a receipt; thread_update
// and thread_group are ordinary tool calls with no request behind them.
// The budget covers a spawn that forks and cuts a worktree, whose
// post-checkout hook runs inside its add (gitops.HookTimeout).
//
//ao:scope terminal:operate
//ao:route selected
//ao:budget 13m
func (a *App) ThreadToolCall(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeTerminalOperate, "start work in a thread"); err != nil {
		return ThreadPeerReply{}, err
	}
	switch call.Tool {
	case "thread_update", "thread_group":
		return a.runThreadPeerTool(ctx, call)
	case "thread_spawn", "thread_send", "thread_ask", "thread_cancel":
		return a.runThreadPeerRequest(ctx, owner, call)
	case threadAgentPeerStop:
		return a.serveThreadAgentPeer(ctx, owner, call)
	case threadOwnerOpTool:
		if !a.ownerOpsOmitted {
			return a.serveThreadOwnerOp(ctx, owner, call)
		}
	case threadAgentPeerPromptReply, threadAgentPeerPromptReturn:
		return a.servePeerPrompt(ctx, owner, call)
	}
	return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("%s is not a thread tools write call.", call.Tool), nil)
}

// ThreadToolRequestStatus answers one source's outstanding set and records
// what it has collected. It is one call per source per tick, so twenty
// open asks on this computer cost one round trip rather than twenty.
//
//ao:scope threads:read
//ao:route selected
func (a *App) ThreadToolRequestStatus(ctx context.Context, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return ThreadPeerPollReply{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "read a thread request"); err != nil {
		return ThreadPeerPollReply{}, err
	}
	if len(poll.Tokens) > threadPeerTokenLimit || len(poll.Ack) > threadPeerTokenLimit || len(poll.Held) > threadPeerTokenLimit {
		return ThreadPeerPollReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("A request poll carries at most %d tokens.", threadPeerTokenLimit), nil)
	}
	var links []OwnerLinkState
	if poll.Launch != "" || len(poll.Links)+len(poll.Contacts)+len(poll.StructuralAck) > 0 {
		links, err = a.readOwnerWork(ctx, owner, poll)
		if err != nil {
			return ThreadPeerPollReply{}, err
		}
	}
	for _, ack := range poll.Ack {
		receipt, found, err := a.store.GetThreadRequestReceipt(ack.Token)
		if err != nil {
			return ThreadPeerPollReply{}, err
		}
		// A token this device does not own is not acknowledged and not
		// reported as an error: the source learns nothing it did not
		// already know, and nothing here is written for it.
		if !found || receipt.OwnerDeviceID != owner {
			continue
		}
		if _, err := a.store.AckThreadRequestReceipt(ack.Token, ack.Revision, 0); err != nil {
			return ThreadPeerPollReply{}, err
		}
	}
	held := make(map[string]int64, len(poll.Held))
	for _, h := range poll.Held {
		held[h.Token] = max(held[h.Token], h.Revision)
	}
	reply := ThreadPeerPollReply{Links: links, Requests: make([]ThreadPeerRequest, 0, len(poll.Tokens))}
	for _, token := range poll.Tokens {
		// Read before the receipt: a handler that is gone by now wrote any
		// receipt it admitted before it went, so the read below finds it.
		admitting := a.peerAdmissions.holds(token)
		if hook := a.peerStatusReadForTest; hook != nil {
			hook(token)
		}
		state, err := a.threadPeerRequestState(ctx, owner, token)
		if err != nil {
			return ThreadPeerPollReply{}, err
		}
		if revision, ok := held[token]; ok && state.Known && revision > 0 && state.Revision > 0 {
			// The source holds the settlement at any revision, and the late
			// reply too at the receipt's own: it takes neither again, so
			// only the texts it lacks are sent.
			state.Answer, state.Cause = "", nil
			if state.Revision <= revision {
				state.LateReply = ""
			}
		}
		state.Admitting = admitting && !state.Known
		reply.Requests = append(reply.Requests, state)
	}
	return reply, nil
}

// ThreadToolExportChunk serves one rendered transcript in transfer-sized
// pieces (readThreadExportChunk): a file that changed since its render is
// refused rather than delivered half old.
//
//ao:scope threads:read
//ao:route selected
//ao:callerbound
func (a *App) ThreadToolExportChunk(ctx context.Context, exportID string, offset int64) (RemoteArtifactChunk, error) {
	if _, err := a.remoteCommandOwner(ctx); err != nil {
		return RemoteArtifactChunk{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "read a thread export"); err != nil {
		return RemoteArtifactChunk{}, err
	}
	return a.readThreadExportChunk(ctx, exportID, offset)
}

// runThreadPeerTool runs one forwarded tool call against this computer's
// own threadtools server.
//
// The session's caller names THIS computer, because its shape, its
// resolution and the computer it stamps on a local row are all this
// computer's. The source's own identity travels separately, on the calls
// that write it into a receipt or a message footer.
func (a *App) runThreadPeerTool(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	caller, err := a.threadPeerCaller(call)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	args := call.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	result, err := a.threadToolsServer().CallForwarded(ctx, caller, call.Tool, args)
	if err != nil {
		return ThreadPeerReply{}, a.publicThreadToolError(call.Tool, caller.ThreadID, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ThreadPeerReply{}, fmt.Errorf("thread tools: encode %s result: %w", call.Tool, err)
	}
	return ThreadPeerReply{Result: encoded}, nil
}

// threadPeerCaller names the calling thread for a forwarded read or
// organize call: the source's thread, this computer's identity.
func (a *App) threadPeerCaller(call ThreadPeerCall) (threadtools.Caller, error) {
	if !entityid.Valid(strings.TrimSpace(call.Source.ThreadID)) {
		return threadtools.Caller{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A forwarded thread tools call must name the thread it came from.", nil)
	}
	backendID, _ := a.backendIdentity()
	return threadtools.Caller{
		ThreadID:     strings.TrimSpace(call.Source.ThreadID),
		Title:        threadPeerTitle(call.Source.Title),
		ComputerID:   backendID,
		ComputerName: a.backendDisplayName(),
	}, nil
}

// threadPeerTitle clips a title another computer chose to the bound this
// computer's own tools enforce. It is rendered into the trusted "Agent
// request" footer of a user message, so its length and its line count are
// this computer's business, not the sender's.
func threadPeerTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if utf8.RuneCountInString(title) <= threadtools.MaxTitleRunes {
		return title
	}
	count := 0
	for index := range title {
		if count == threadtools.MaxTitleRunes {
			return title[:index]
		}
		count++
	}
	return title
}

// refuseForeignLedgerRead refuses a call that reads the CALLER's own request
// ledger when the caller is another computer's thread.
//
// A forwarded call names a thread on the computer it came from, and this
// computer answers it with its own rows: those two meanings of "the caller's
// thread" are the same only for a local call. A forwarded `thread_status`
// with tokens or with no arguments would read whatever local thread the
// sender named, so it is refused here rather than answered. The thread_ids
// branch is the one that is legitimately forwarded, and it reads threads
// rather than the ledger.
func refuseForeignLedgerRead(ctx context.Context, what string) error {
	if !threadtools.Forwarded(ctx) {
		return nil
	}
	return errorsx.Public(threadtools.CodeInvalidRequest,
		"A request record belongs to the computer whose thread made it, so "+what+
			" cannot be read from another computer. Call thread_status on your own computer.", nil)
}

// refuseForwardedHomeFiles refuses a forwarded spawn or send that still names
// home_files: the files are read on the caller's computer and folded into the
// text before it is forwarded, and this computer holds no home of the
// caller's to read them from.
func refuseForwardedHomeFiles(files []string) error {
	if len(files) == 0 {
		return nil
	}
	return errorsx.Public(threadtools.CodeInvalidRequest,
		"home_files are read on the sender's computer and cannot be forwarded. Send the files' text in the message.", nil)
}

// publicThreadToolError keeps an unreviewed cause out of the wire. The
// code and prose reach the calling model on the other computer, so an
// internal failure is logged here and answered with a stable refusal.
func (a *App) publicThreadToolError(tool, threadID string, err error) error {
	if _, _, public := errorsx.PublicDetails(err); public {
		return err
	}
	logThreadRequestSweep("serve "+tool+" for "+threadID, err)
	return errorsx.Public(threadtools.CodeInvalidRequest,
		"Thread tools could not complete that call on this computer.", err)
}

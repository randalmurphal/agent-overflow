package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/threadtools"
)

// Cross-computer reach for the agent thread tools: the paired computers
// the tool shapes are computed from, the client every forwarded call goes
// through, and the error shape a peer failure reaches the model in.
//
// Reach is pairing alone. The agent-commands opt-in guards a shell on the
// destination; nothing here runs one, and the destination authorizes every
// call itself against the authenticated device.

// threadPeerCallTimeout bounds one forwarded call. A read of a long thread
// renders a page on the destination and a write starts a turn there, so it
// sits well above a round trip and well below the tool call ceiling.
const threadPeerCallTimeout = 60 * time.Second

// PairedComputers lists the computers this one is paired with, never
// including this computer. An empty list is the single-computer shape, in
// which no schema carries computer_id and no result row carries a
// computer field.
func (t threadToolsApp) PairedComputers(context.Context) ([]threadtools.Computer, error) {
	if t.app == nil || t.app.backends == nil {
		return nil, nil
	}
	rows, err := t.app.backends.List()
	if err != nil {
		return nil, err
	}
	self, _ := t.app.backendIdentity()
	out := make([]threadtools.Computer, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" || row.ID == self {
			continue
		}
		out = append(out, threadtools.Computer{ID: row.ID, Name: attachedComputerName(row)})
	}
	return out, nil
}

// attachedComputerName is what this computer calls a paired one: the
// nickname the user gave it, else the name that computer calls itself.
func attachedComputerName(row attachedbackends.Attached) string {
	if name := strings.Join(strings.Fields(row.Nickname), " "); name != "" {
		return name
	}
	return strings.Join(strings.Fields(row.Name), " ")
}

// Peer returns a client for one paired computer. An unknown or unpaired
// id is a public refusal, because it reaches the model as an errors row.
func (t threadToolsApp) Peer(ctx context.Context, computerID string) (threadtools.Peer, error) {
	computers, err := t.PairedComputers(ctx)
	if err != nil {
		return nil, errorsx.Public(threadtools.CodeUnreachable,
			"This computer could not read its pairings. Check Remote access on it and try again.", err)
	}
	source, _ := threadtools.CallerFrom(ctx)
	for _, computer := range computers {
		if computer.ID == computerID {
			return threadPeerClient{app: t.app, computer: computer, source: source}, nil
		}
	}
	return nil, errorsx.Public(threadtools.CodeUnreachable,
		fmt.Sprintf("Computer %s is not paired with this computer. thread_options lists the computers it can reach.", computerID), nil)
}

// threadPeerClient is one paired computer as threadtools reaches it. Every
// method is one authenticated RPC on the pairing's own rotating
// credential, bounded by its own timeout so one unreachable computer costs
// the fan-out one slot rather than the whole call.
type threadPeerClient struct {
	app      *App
	computer threadtools.Computer
	// source is the calling thread as this computer names it. The
	// destination records it for display and attribution; it authorizes
	// nothing, which the authenticated device does.
	source threadtools.Caller
}

func (p threadPeerClient) Computer() threadtools.Computer { return p.computer }

func (p threadPeerClient) Resolve(ctx context.Context, ref string) (threadtools.Resolution, error) {
	call, cancel := context.WithTimeout(ctx, threadPeerCallTimeout)
	defer cancel()
	var answer threadtools.Resolution
	if err := p.app.backends.CallThreadPeer(call, p.computer.ID, "ThreadToolResolve", &answer,
		ThreadPeerResolve{Ref: ref, Source: p.source}); err != nil {
		return threadtools.Resolution{}, p.app.threadOperationError("resolve", p.computer.ID, ref, err)
	}
	return answer, nil
}

func (p threadPeerClient) Query(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return p.forward(ctx, "ThreadToolQuery", name, args)
}

func (p threadPeerClient) Invoke(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return p.forward(ctx, "ThreadToolCall", name, args)
}

func (p threadPeerClient) forward(ctx context.Context, method, name string, args json.RawMessage) (json.RawMessage, error) {
	call, cancel := context.WithTimeout(ctx, threadPeerCallTimeout)
	defer cancel()
	var reply ThreadPeerReply
	err := p.app.backends.CallThreadPeer(call, p.computer.ID, method, &reply,
		ThreadPeerCall{Tool: name, Args: args, Source: p.source})
	if err != nil {
		return nil, p.app.threadOperationError(name, p.computer.ID, "", err)
	}
	if len(reply.Result) == 0 {
		return nil, errorsx.Public(threadtools.CodeUnreachable,
			fmt.Sprintf("%s answered %s with no result.", threadtools.NameOfComputer(p.computer), name), nil)
	}
	return reply.Result, nil
}

// FetchExport copies a file the destination rendered into this computer's
// own export directory, so the path the model reads is one it can open.
func (p threadPeerClient) FetchExport(ctx context.Context, file threadtools.ExportFile) (threadtools.ExportFile, error) {
	return p.app.fetchThreadExport(ctx, p.computer, file)
}

// threadOperationError names the operation, the computer and the thread a
// refusal addressed, in the remote-commands shape: a stable code, prose a
// model can act on, and the ids beside the names so a retry can carry
// them. Wrapping is idempotent; raw causes stay in the host log behind a
// reference id.
func (a *App) threadOperationError(action, computerID, threadID string, err error) error {
	var done remoteContextError
	if err == nil || errors.As(err, &done) {
		return err
	}
	code, message, _ := threadErrorDetails(action, err)
	// Verbs read "Thread cancel on X"; a tool name already names the
	// thread, so it reads "thread_spawn on X".
	prefix := "Thread " + action
	if strings.HasPrefix(action, "thread_") {
		prefix = action
	}
	if entityid.Valid(computerID) {
		if name := a.remoteComputerNames()[computerID]; name != "" {
			prefix += " on " + name + " (computer " + computerID + ")"
		} else {
			prefix += " on computer " + computerID
		}
	}
	if threadID != "" {
		prefix += " for thread " + threadID
	}
	return remoteContextError{errorsx.Public(code, fmt.Sprintf("%s: %s", prefix, message), err)}
}

// threadErrorDetails classifies one peer failure. A refusal the
// destination wrote with a thread tools code is reviewed public prose
// already and crosses unchanged, and it is certain: the destination
// answered. Everything else goes through the remote classification, so
// pairing, connection and timeout failures read the same on both
// surfaces, and uncertain means the destination may have acted.
func threadErrorDetails(action string, err error) (code, message string, uncertain bool) {
	var remote *rpcclient.Error
	if errors.As(err, &remote) && strings.HasPrefix(remote.Code, "thread_") {
		return remote.Code, remote.Message, false
	}
	if public, text, ok := errorsx.PublicDetails(err); ok && strings.HasPrefix(public, "thread_") {
		return public, text, false
	}
	return remoteErrorDetails(action, err)
}

// threadRequestNeverSent reports whether a dispatch failure happened
// before the destination could see the request. These are the refusals
// attachedbackends raises while addressing the call, loading the pairing or
// opening the RPC, so the source row is settled `refused`
// (settleUnconfirmedRequest) instead of left unconfirmed for a poller that
// would only collect the same answer.
//
// threadtools.CodeUnreachable is deliberately NOT in the set, close as it
// reads: it is also what this computer raises for a destination that DID
// answer, without a usable result (threadPeerClient.forward,
// callThreadPeerRequest). Those requests may be running on the other
// computer, and settling one refused would tell the caller nothing happened
// while the work continues.
func threadRequestNeverSent(code string) bool {
	switch code {
	case attachedbackends.CodeThreadUnsupported, attachedbackends.CodeThreadUnreachable,
		"remote_not_paired", "remote_pairing_pending", "remote_pairing_unavailable",
		"remote_pairing_expired":
		return true
	}
	return false
}

// threadPairingEnded reports the refusals that mean this pairing will not
// answer again until the user reconnects it. An open request against such
// a computer is settled rather than retried forever.
func threadPairingEnded(code string) bool {
	switch code {
	case "remote_not_paired", "remote_pairing_expired":
		return true
	}
	return false
}

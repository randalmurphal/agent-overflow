package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider/codex"
)

// codexReviewRPCTimeout bounds the Wails-side wait on `review/start` and
// `thread/compact/start`. Both responses are acknowledgements — the review's
// transcript and the compaction divider arrive later as ordinary notifications
// — so a slow answer means a wedged subprocess, not a long-running job. Same
// budget and same reasoning as cleanCodexBackgroundTerminalsTimeout.
const codexReviewRPCTimeout = 10 * time.Second

// CodexReviewTarget is the wire form of the closed union `review/start` takes.
//
// Flat with a Kind discriminator because that is what survives a TypeScript
// binding cleanly; the four variants' different required payloads are validated
// on the way back into codex.ReviewTarget, whose constructors are the only way
// to build one. A field belonging to another variant is ignored rather than
// rejected — the discriminator decides what the request means, and a composer
// that keeps a stale branch name in its form state while the user switches to
// "uncommitted changes" is not making an error.
type CodexReviewTarget struct {
	// Kind is one of uncommittedChanges | baseBranch | commit | custom.
	Kind string `json:"kind"`
	// Branch is required for baseBranch.
	Branch string `json:"branch,omitempty"`
	// SHA is required for commit.
	SHA string `json:"sha,omitempty"`
	// Title is the optional human label for a commit target.
	Title string `json:"title,omitempty"`
	// Instructions is required for custom.
	Instructions string `json:"instructions,omitempty"`
}

// CodexReviewStarted is what StartCodexReview answers with.
//
// ThreadID is the AO thread the review runs on. It is reported back rather than
// assumed because the whole point of returning it is that the caller does not
// have to derive routing from the request: the review is an ordinary turn on
// this thread and its transcript arrives on the channels the thread already
// subscribes to.
type CodexReviewStarted struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	TurnStatus string `json:"turnStatus"`
}

// StartCodexReview runs Codex's built-in code review on the thread's current
// workspace state, INLINE — the review turn lands on this thread, so its
// transcript flows through the same triage path every other turn does.
//
// Detached delivery is deliberately not exposed. A detached review runs on a
// thread this session does not own, so every notification it produces hits the
// child-thread quarantine and is dropped; surfacing one needs the returned
// review thread id registered with the routing tables first, which is not
// wired. See codex.Session.StartReview.
//
// The review is a real, billed turn and it is not steerable, so callers should
// offer it only on an idle thread.
//
// LocalOnly: it drives the thread's live provider subprocess.
func (a *App) StartCodexReview(ctx context.Context, threadID string, target CodexReviewTarget) (CodexReviewStarted, error) {
	if a.shuttingDown.Load() {
		return CodexReviewStarted{}, ErrShuttingDown
	}
	sess, err := a.codexSessionForThread("start codex review", threadID)
	if err != nil {
		return CodexReviewStarted{}, err
	}
	reviewTarget, err := codexReviewTargetFromWire(target)
	if err != nil {
		return CodexReviewStarted{}, fmt.Errorf("app: start codex review: %w", err)
	}

	rpcCtx, cancel := context.WithTimeout(ctx, codexReviewRPCTimeout)
	defer cancel()
	started, err := sess.StartReview(rpcCtx, reviewTarget, codex.ReviewDeliveryInline)
	if err != nil {
		return CodexReviewStarted{}, fmt.Errorf("app: start codex review: %w", err)
	}
	if started.Detached {
		// We asked for inline and the server answered with a thread this
		// session does not own. Its notifications are quarantined, so the
		// review would burn tokens and produce nothing visible. Say so
		// rather than returning a success the UI cannot act on.
		return CodexReviewStarted{}, fmt.Errorf(
			"app: start codex review: codex answered with detached review thread %s; its transcript cannot be shown",
			started.ReviewThreadID,
		)
	}
	return CodexReviewStarted{
		ThreadID:   threadID,
		TurnID:     started.TurnID,
		TurnStatus: started.TurnStatus,
	}, nil
}

// CompactCodexThread asks Codex to compact the thread's context now.
//
// The response is an acknowledgement only. The observable result is the
// `contextCompaction` thread item, which triage already routes to the
// transcript's compaction divider — nothing to correlate here.
//
// Compaction runs as a non-steerable turn, so like a review it belongs on an
// idle thread.
//
// LocalOnly: it drives the thread's live provider subprocess.
func (a *App) CompactCodexThread(ctx context.Context, threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	sess, err := a.codexSessionForThread("compact codex thread", threadID)
	if err != nil {
		return err
	}
	rpcCtx, cancel := context.WithTimeout(ctx, codexReviewRPCTimeout)
	defer cancel()
	if err := sess.CompactThread(rpcCtx); err != nil {
		return fmt.Errorf("app: compact codex thread: %w", err)
	}
	return nil
}

// codexSessionForThread resolves the live Codex session driving one thread.
//
// Never starts one. Both callers steer an EXISTING conversation — a review or a
// compaction of a thread with no process behind it has no context to act on —
// so a missing session is a user-facing "there is nothing running here", not an
// invitation to spawn a subprocess the user did not ask for.
func (a *App) codexSessionForThread(action, threadID string) (*codex.Session, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, fmt.Errorf("app: %s: empty thread id", action)
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return nil, fmt.Errorf("app: %s: no active session for thread %s; send a message first", action, threadID)
	}
	if sess.codex == nil {
		return nil, fmt.Errorf("app: %s: thread %s is not a Codex thread", action, threadID)
	}
	return sess.codex, nil
}

// codexReviewTargetFromWire rebuilds the closed union from its flat wire form,
// routing every variant through the package's own validating constructors so a
// missing sha or branch is refused here rather than marshalled into a request
// Codex would answer about the wrong thing.
func codexReviewTargetFromWire(target CodexReviewTarget) (codex.ReviewTarget, error) {
	switch codex.ReviewTargetKind(strings.TrimSpace(target.Kind)) {
	case codex.ReviewTargetUncommittedChanges:
		return codex.ReviewUncommittedChanges(), nil
	case codex.ReviewTargetBaseBranch:
		return codex.ReviewBaseBranch(target.Branch)
	case codex.ReviewTargetCommit:
		return codex.ReviewCommit(target.SHA, target.Title)
	case codex.ReviewTargetCustom:
		return codex.ReviewCustom(target.Instructions)
	default:
		return codex.ReviewTarget{}, fmt.Errorf("unknown review target kind %q", target.Kind)
	}
}

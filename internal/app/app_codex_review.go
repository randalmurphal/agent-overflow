package app

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

// StartCodexReview runs Codex's built-in review through the normal composer
// send transaction. The user command, turn, nested agent activity, sourced
// result, lazy session start, and send-failure state therefore share one path.
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
	thread, err := a.store.GetThread(strings.TrimSpace(threadID))
	if err != nil {
		return CodexReviewStarted{}, fmt.Errorf("app: start codex review: load thread: %w", err)
	}
	if thread.Provider != "codex" {
		return CodexReviewStarted{}, fmt.Errorf("app: start codex review: thread %s is not a Codex thread", thread.ID)
	}
	reviewTarget, err := codexReviewTargetFromWire(target)
	if err != nil {
		return CodexReviewStarted{}, fmt.Errorf("app: start codex review: %w", err)
	}
	command := codexReviewCommandText(reviewTarget)
	var started codex.ReviewStarted
	_, err = a.sendMessageWithOptions(ctx, threadID, command, sendMessageOptions{
		ExpandComposerCommands: true,
		onCodexReviewStarted: func(observed codex.ReviewStarted) {
			started = observed
		},
	})
	if err != nil {
		return CodexReviewStarted{}, fmt.Errorf("app: start codex review: %w", err)
	}
	return CodexReviewStarted{
		ThreadID:   threadID,
		TurnID:     started.TurnID,
		TurnStatus: started.TurnStatus,
	}, nil
}

func codexReviewCommandText(target codex.ReviewTarget) string {
	switch target.Kind() {
	case codex.ReviewTargetBaseBranch:
		return "/review branch " + target.Branch()
	case codex.ReviewTargetCommit:
		command := "/review commit " + target.SHA()
		if title := target.Title(); title != "" {
			command += " " + title
		}
		return command
	case codex.ReviewTargetCustom:
		return "/review custom " + target.Instructions()
	default:
		return "/review"
	}
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
// It never starts one. Manual compaction operates only on an existing provider
// context. Review uses the normal send path and can lazy-start the session.
func (a *App) codexSessionForThread(action, threadID string) (*codex.Session, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, fmt.Errorf("app: %s: empty thread id", action)
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return nil, fmt.Errorf("app: %s: no active session for thread %s; send a message first", action, threadID)
	}
	if sess.Codex == nil {
		return nil, fmt.Errorf("app: %s: thread %s is not a Codex thread", action, threadID)
	}
	return sess.Codex, nil
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

// codexReviewCommandTarget recognises the built-in turn command from the exact
// user-authored text. A leading space or any command word other than /review
// is ordinary model input. The grammar mirrors the composer target picker.
func codexReviewCommandTarget(content string) (codex.ReviewTarget, bool, error) {
	if content != strings.TrimLeft(content, " \t\r\n") || !strings.HasPrefix(content, "/review") {
		return codex.ReviewTarget{}, false, nil
	}
	if len(content) > len("/review") {
		next := content[len("/review")]
		if next != ' ' && next != '\t' && next != '\r' && next != '\n' {
			return codex.ReviewTarget{}, false, nil
		}
	}
	arg := strings.TrimSpace(content[len("/review"):])
	if arg == "" || arg == "uncommitted" {
		return codex.ReviewUncommittedChanges(), true, nil
	}
	fields := strings.Fields(arg)
	head := fields[0]
	rest := strings.TrimSpace(arg[len(head):])
	switch head {
	case "branch":
		branch := ""
		if restFields := strings.Fields(rest); len(restFields) > 0 {
			branch = restFields[0]
		}
		target, err := codex.ReviewBaseBranch(branch)
		return target, true, err
	case "commit":
		sha := ""
		title := ""
		if restFields := strings.Fields(rest); len(restFields) > 0 {
			sha = restFields[0]
			title = strings.TrimSpace(rest[len(sha):])
		}
		target, err := codex.ReviewCommit(sha, title)
		return target, true, err
	case "custom":
		target, err := codex.ReviewCustom(rest)
		return target, true, err
	default:
		return codex.ReviewTarget{}, true, fmt.Errorf(
			"unknown review target %q; use uncommitted, branch, commit, or custom", head,
		)
	}
}

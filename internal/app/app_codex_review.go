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
//
//ao:scope threads:operate
func (a *App) CompactCodexThread(ctx context.Context, threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	sess, err := a.codexSessionForThread("compact codex thread", threadID)
	if err != nil {
		return err
	}
	// The request starts a compaction turn, so once sent it runs to its own
	// timeout rather than the caller's.
	rpcCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), codexReviewRPCTimeout)
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

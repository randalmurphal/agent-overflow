package codex

import (
	"context"
	"fmt"
	"log"

	"agent-overflow/internal/provider"
)

// session_start.go — the sequence that turns a spawned `codex app-server`
// process into a session with a thread: the constructor (spawn, `initialize`
// handshake and its version check, `initialized`), the
// `BeforeResume` window a resume opens before the thread is loaded, and the
// one `thread/start` / `thread/resume` RPC plus everything that has to be
// true about its response — the paginated-history downgrade retry, the
// thread-identity and approvals-reviewer echoes, the history-mode record,
// and a resume's collab rehydration.
//
// The Session struct these two build, and its teardown, are in session.go;
// the turn verbs a live session then serves are in session_turn.go.

// NewSession spawns a dedicated codex app-server Conn, performs the initialize
// handshake, and opens a thread on it. Closing the returned Session closes
// that dedicated Conn. For multiplexed production sessions, use Dial + Conn.Open.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	if err := checkApprovalPolicy(cfg.ApprovalPolicy); err != nil {
		return nil, err
	}
	if _, err := provider.NewSessionStartDelivery(cfg.SessionStartHook, cfg.SessionStartForget); err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	conn, err := Dial(ctx, ConnConfig{
		Binary:      cfg.Binary,
		WorkDir:     cfg.WorkDir,
		Env:         cfg.Env,
		EventLogger: cfg.EventLogger,
		Reaper:      cfg.Reaper,
		ThreadID:    threadID,
	})
	if err != nil {
		return nil, err
	}
	sess, err := conn.open(ctx, threadID, cfg, onEvent, true, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return sess, nil
}

// startOrResumeThread runs the ONE RPC that gives this session a thread, and
// everything that has to be true about its response before the session is
// handed to a caller: the thread-identity echo, the approvals-reviewer echo,
// the history-mode record, and — on a resume — collab rehydration.
//
// It is separate from NewSession because these are the session's PRECONDITIONS
// rather than its construction: every failure here means "there is no usable
// thread", and the caller answers all of them the same way (Close, propagate).
// Interleaved with the spawn and handshake they read as a second constructor.
// Nothing here touches the send or queue paths.
func (s *Session) startOrResumeThread(ctx context.Context, cfg Config) error {
	// Composed before the params so a failed config read omits the
	// override rather than replacing the user's configured value.
	developerInstructions := s.composeDeveloperInstructions(ctx, cfg.DeveloperInstructions)
	cfg.DeveloperInstructions = developerInstructions.override
	s.setDeveloperInstructions(cfg.DeveloperInstructions)
	s.mu.Lock()
	s.developerInstructionsComposed = developerInstructions
	s.mu.Unlock()

	threadParams := buildThreadParams(cfg)
	s.mu.Lock()
	s.rehydrateParams = threadRehydrateParams(threadParams)
	s.mu.Unlock()

	var method string
	if cfg.ResumeThreadID != "" {
		method = "thread/resume"
		threadParams["threadId"] = cfg.ResumeThreadID
		// AO owns its rendered history in SQLite. Returning thread.turns here
		// makes a cold resume one unbounded NDJSON frame and duplicates content
		// the app already has; one long-running turn can therefore exceed the
		// provider line safety limit by itself. Codex has supported this field
		// throughout AO's supported range (0.143+), for legacy and paginated
		// histories alike.
		threadParams["excludeTurns"] = true
	} else {
		method = "thread/start"
		threadParams["experimentalRawEvents"] = true
		// A new thread's persisted history contract is decided HERE and
		// nowhere else: `thread/resume` has no such field (a resumed thread
		// keeps the mode its rollout recorded), and upstream's default is
		// legacy (`ThreadHistoryMode::default()`). Asking for paginated is
		// what makes the in-place `thread/revert` cut available to this
		// thread for the rest of its life (session_revert.go).
		threadParams["historyMode"] = paginatedThreadHistoryMode
	}

	// Presence, not truthiness: the retry below has to know whether AO ASKED
	// for a history mode, and a value test conflates "did not ask" with a
	// mode that happens to read as empty. Only this function writes the key,
	// and only with a non-empty value — but the downgrade retry is the one
	// place that must not depend on that staying true.
	_, asked := threadParams["historyMode"]
	// Armed before the request: the read loop is live, and a turn the
	// app-server starts right after its answer (a queued message, a resumed
	// running turn) runs the hooks before this goroutine reads the answer.
	// A resumed thread carries the hooks' earlier output in its history and
	// runs none at its resume; a new thread runs every hook in its first
	// turn.
	if method != "thread/resume" {
		s.sessionStart.Expect(provider.SessionStartAfterStart)
	}

	resp, err := s.sendRequest(ctx, method, threadParams)
	if err != nil && asked && isHistoryPaginationUnsupported(err) {
		// The paginated refusal is raised while destructuring the params,
		// before any thread exists, so this is a retry rather than a second
		// half-start. Mirrors upstream's own client downgrade
		// (request_thread_start_with_history_fallback, rust-v0.149.0
		// codex-rs/tui/src/app_server_session.rs:202).
		log.Printf("codex: thread/start refused paginated history (%v); retrying with the server default", err)
		delete(threadParams, "historyMode")
		resp, err = s.sendRequest(ctx, method, threadParams)
	}
	if err != nil {
		// A thread another Codex process owns refuses here with a raw
		// "thread <uuid> already has an active writer"; classify it so the
		// session-start failure the user sees names the TUI, not the lock.
		return fmt.Errorf("codex: %s failed: %w", method, classifyThreadWriterConflict(err))
	}

	// Extract the Codex thread ID from response. s.threadID is already set
	// by the constructor; re-assigning it here would be a write racing every
	// read-loop read of it for no change in value.
	responseThreadID := readNestedString(resp, "thread", "id")
	if responseThreadID == "" {
		log.Printf("codex: %s response missing thread.id; response: %s", method, string(resp))
		return fmt.Errorf("codex: %s: response did not contain a thread ID", method)
	}
	if seeded := s.rootThreadID(); seeded != "" && seeded != responseThreadID {
		return fmt.Errorf("codex: %s: response thread ID %q does not match requested thread %q", method, responseThreadID, seeded)
	}
	if err := verifyApprovalsReviewerEcho(method, threadApprovalsReviewer(cfg), resp); err != nil {
		return err
	}
	s.setRootThreadID(responseThreadID)
	s.setRootSessionID(readNestedString(resp, "thread", "sessionId"))
	// The start/resume response is the only place a thread states its
	// persisted history contract; `thread/revert` is available on one of
	// the two (session_revert.go).
	s.recordThreadHistoryMode(resp)
	if method == "thread/resume" {
		// The rollout path is recorded BEFORE rehydration: rehydration puts
		// work on the collab worker and the read loop is already live, so a
		// spawn can arm the tail from this moment on and needs the path in
		// place. Recording is not arming — see sessionRolloutTailState.
		s.prepareRolloutSubagentNotificationTail(readNestedString(resp, "thread", "path"))
		s.rehydrateCollabOwnership(cfg.ResumeCollabLaunches)
		s.discoverResumedChildren()
		if cfg.ResumeHasUnresolvedSubagents || len(cfg.ResumeCollabLaunches) > 0 {
			// The app layer found spawn launches on this thread that are still
			// waiting for their answer, so the mailbox delivery this session
			// cannot see as a raw event is exactly what it is about to miss.
			s.armRolloutSubagentNotificationTail("resume with unresolved spawn children")
		}
		// Before the session is handed to a caller, so no turn runs on the
		// earlier instructions. A failure leaves the session usable.
		s.reportDeveloperInstructionsFailure(s.ensureDeveloperInstructions(ctx, developerInstructions))
		s.reportResumedThreadMemoriesFailure(s.disableResumedThreadMemories(ctx))
	} else {
		if s.developerInstructionsRecord != nil && developerInstructions.known {
			// A new thread reads the composed text in its initial context.
			s.reportDeveloperInstructionsFailure(s.recordDeveloperInstructions(DeveloperInstructionsDigest(developerInstructions.text)))
		}
	}

	return nil
}

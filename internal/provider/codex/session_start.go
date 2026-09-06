package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
)

// session_start.go — the sequence that turns a spawned `codex app-server`
// process into a session with a thread: the constructor (spawn, `initialize`
// handshake, version + queue-support records, `initialized`), the
// `BeforeResume` window a resume opens before the thread is loaded, and the
// one `thread/start` / `thread/resume` RPC plus everything that has to be
// true about its response — the paginated-history downgrade retry, the
// thread-identity and approvals-reviewer echoes, the history-mode record,
// and a resume's collab rehydration.
//
// The Session struct these two build, and its teardown, are in session.go;
// the turn verbs a live session then serves are in session_turn.go.

// NewSession spawns codex app-server, performs the initialize handshake,
// and starts (or resumes) a thread. Returns after handshake completes.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "codex"
	}

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary:           binary,
		Args:             codexAppServerArgs(),
		Dir:              cfg.WorkDir,
		Env:              cfg.Env,
		UnsetEnv:         []string{"CODEX_HOME"},
		EventLogger:      cfg.EventLogger,
		EventLogRedactor: newCodexProviderEventLogRedactor(),
		ThreadID:         threadID,
		Provider:         string(provider.Codex),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codex: spawn: %w", err)
	}

	s := &Session{
		proc:      proc,
		ctx:       childCtx,
		threadID:  threadID,
		workDir:   cfg.WorkDir,
		binary:    binary,
		usageAcct: newUsageAccounting(cfg.ResumeThreadID != ""),
		turnConfig: sessionTurnConfig{
			model:               cfg.Model,
			reasoningEffort:     cfg.ReasoningEffort,
			serviceTier:         cfg.ServiceTier,
			assertedServiceTier: cfg.ServiceTier,
			approvalPolicy:      cfg.ApprovalPolicy,
			sandbox:             cfg.Sandbox,
			approvalsReviewer:   threadApprovalsReviewer(cfg),
		},
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  onEvent,
		cancel:   cancel,
		readDone: make(chan struct{}),
		collab: sessionCollabState{
			childParentByThread:       make(map[string]string),
			childParentByAgentPath:    make(map[string]string),
			childThreadByAgentPath:    make(map[string]string),
			childPathOwnerLive:        make(map[string]bool),
			agentPathByThread:         make(map[string]string),
			agentMetaByThread:         make(map[string]collabReceiverMeta),
			subagentNotificationDedup: make(map[subagentNotificationDedupKey]struct{}),
			childRuntimeByThread:      make(map[string]childRuntimeState),
		},
		collabMetadataReads: make(chan struct{}, 4),
		rawCalls: sessionRawToolCallState{
			byID:                  make(map[string]rawToolCall),
			waitReceiverIDsByCall: make(map[string][]string),
		},
		childRouting: sessionChildRoutingState{
			deferredChildWireEvents: make(map[string][]deferredChildWireEvent),
			deferredChildDeadlines:  make(map[string]*time.Timer),
		},
		collabHistory: sessionCollabHistoryState{
			generation: 1,
			visited:    make(map[string]uint64),
		},
		planBuffersByItemID: make(map[string]*planBuffer),
		planBuffersByTurnID: make(map[string]*planBuffer),
		ownsQueuedClient:    cfg.OwnsQueuedClientID,
	}
	// On resume the root provider id is already durable in AO. Seed it before
	// the read loop starts so child notifications racing the thread/resume
	// response are quarantined instead of being mistaken for root events. A
	// fresh thread cannot have children before NewSession returns.
	if cfg.ResumeThreadID != "" {
		s.setRootThreadID(cfg.ResumeThreadID)
	}

	// Start stdout reader goroutine before sending any requests.
	go s.readLoop()

	// Initialize handshake. The opt-out list is the complement of what
	// this package consumes, so Codex stops emitting the ~30 notification
	// methods we would otherwise parse, route and drop per app-server
	// (one per thread).
	initializeResp, err := s.sendRequest(ctx, "initialize",
		codexInitializeParams(provider.CodexClientOrigin, sessionOptOutNotificationMethods()))
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: initialize handshake failed: %w", err)
	}
	// The handshake response is the only place a live app-server states its
	// own build. Per-method version floors read it; never a second probe.
	s.recordAppServerVersion(initializeResp)
	// Same handshake, one derived decision: which queue owns a message the
	// user sends while this session is busy. Frozen here so nothing can
	// observe it change mid-session (thread_queue.go).
	s.recordThreadQueueSupport()

	// Send initialized notification (no id, no response expected).
	if err := s.writeNotification("initialized", nil); err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: send initialized notification: %w", err)
	}

	// The last moment before the thread is loaded — see Config.BeforeResume.
	if cfg.ResumeThreadID != "" && cfg.BeforeResume != nil {
		cfg.BeforeResume(s)
	}

	if err := s.startOrResumeThread(ctx, cfg); err != nil {
		s.Close()
		return nil, err
	}

	meta, _ := json.Marshal(provider.SessionInfo{
		SessionID: s.rootThreadID(),
		Model:     cfg.Model,
		CWD:       cfg.WorkDir,
	})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	})

	return s, nil
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
	// The version comes from the handshake recorded moments ago — the
	// connected process's own statement of its build.
	threadParams := buildThreadParams(cfg, s.AppServerVersion())
	if cfg.AdditionalInstructions != "" {
		instructions, err := s.appendDeveloperInstructions(ctx, cfg.AdditionalInstructions)
		if err != nil {
			return err
		}
		threadParams["developerInstructions"] = instructions
	}
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
		// nowhere else — `thread/resume` has no such field, and upstream's
		// default is legacy. Asking for paginated is what makes the
		// in-place `thread/revert` cut available to this thread for the
		// rest of its life (session_revert.go).
		if historyMode := threadStartHistoryMode(s.AppServerVersion()); historyMode != "" {
			threadParams["historyMode"] = historyMode
		}
	}

	// Presence, not truthiness: the retry below has to know whether AO ASKED
	// for a history mode, and a value test conflates "did not ask" with a
	// mode that happens to read as empty. Only this function writes the key,
	// and only with a non-empty value — but the downgrade retry is the one
	// place that must not depend on that staying true.
	_, asked := threadParams["historyMode"]

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
		if cfg.ResumeHasUnresolvedSubagents {
			// The app layer found spawn launches on this thread that are still
			// waiting for their answer, so the mailbox delivery this session
			// cannot see as a raw event is exactly what it is about to miss.
			s.armRolloutSubagentNotificationTail("resume with unresolved spawn children")
		}
	}

	return nil
}

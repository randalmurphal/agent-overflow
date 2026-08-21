package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/harness/control"
)

// The Codex thread's HISTORY CONTRACT and the in-place cut that depends
// on it (`thread/revert`, app-server >= 0.148).
//
// Upstream keeps two history modes on a thread and picks between them at
// `thread/start` (rust-v0.149.0
// codex-rs/app-server-protocol/src/protocol/v2/thread_data.rs). "legacy"
// is the DEFAULT — a client that says nothing gets it — and
// `thread/revert` is refused outright on such a thread
// (codex-rs/app-server/src/request_processors/thread_processor.rs
// `thread_revert_response`). That refusal is the whole reason this mock
// has to model the mode at all: AO now asks for `historyMode:
// "paginated"` whenever the handshake reports >= 0.148 and falls back to
// `thread/fork` when the thread is legacy, so a mock that echoed nothing
// would send every harness rollback down the fork path and the in-place
// cut would never be exercised.
//
// Two rules make the model truthful rather than merely convenient:
//
//   - The mode is decided ONCE, at `thread/start`, from the params.
//     `ThreadResumeParams` has no history-mode member at all, so a resume
//     can only report what the thread already is.
//   - It therefore has to OUTLIVE the process. Every real rollback cuts
//     through a throwaway resume session (app_thread_fork_codex.go
//     `withCodexThreadSession`), which is a second mock process that
//     never saw the start. A mode held only in memory would read as
//     legacy there and the fork fallback would fire on a thread that is
//     genuinely paginated — the exact bug this file exists to make
//     impossible. The store below is the mock's miniature of upstream's
//     thread store, and like the Claude adapter's transcript it exists
//     only when a fully isolated harness boot hands over a home
//     (control.EnvTranscriptHome). Without one the mock keeps the mode in
//     memory for the life of the connection and a resume reads legacy,
//     which fails CLOSED.
const (
	legacyHistoryMode    = "legacy"
	paginatedHistoryMode = "paginated"

	// revertRefusalMessage is upstream's verbatim text. AO does not parse
	// it (classifyThreadRevertError keys off the -32600 code plus this
	// wording), but a mock that paraphrased it would stop testing the
	// classifier at all.
	revertRefusalMessage = "thread/revert only supports paginated threads"
)

// historyModeFromStartParams reads the requested mode, defaulting to
// upstream's own default. An unknown value is passed through rather than
// corrected: the app should never send one, and silently normalising it
// would hide the day it does.
func historyModeFromStartParams(params json.RawMessage) string {
	mode := strings.TrimSpace(readParamString(params, "historyMode"))
	if mode == "" {
		return legacyHistoryMode
	}
	return mode
}

// noteThreadStart records the history mode a `thread/start` asked for and
// persists it under the started thread's id.
func (a *codexAdapter) noteThreadStart(params json.RawMessage) {
	mode := historyModeFromStartParams(params)
	a.mu.Lock()
	a.historyMode = mode
	a.resumedThread = false
	a.mu.Unlock()
	a.persistHistoryMode(a.e.currentVars()["THREAD_ID"], mode)
}

// noteThreadResume loads the history mode the thread was STARTED with.
// A thread this harness never started (or one started before the home
// existed) reads legacy, matching a rollout that predates the paginated
// opt-in.
func (a *codexAdapter) noteThreadResume(threadID string) {
	mode := a.loadHistoryMode(threadID)
	a.mu.Lock()
	a.historyMode = mode
	a.resumedThread = true
	a.mu.Unlock()
}

// anchorIsCuttable reports whether a history anchor is one this connection
// is willing to cut at, for either of the two cuts.
//
// Three cases, and only the middle one is a real refusal:
//
//   - A turn this process began and FINISHED. Cuttable, and the only
//     anchor the mock can vouch for out of its own ledger.
//   - A turn this process began and has NOT finished. Codex refuses that
//     outright — `thread/fork` because the snapshot would be of a live
//     turn (the refusal AO's mid-turn anchor normalisation exists for),
//     `thread/revert` because upstream would tear the runtime down and
//     silently destroy the turn (AO refuses to reach it mid-turn, and a
//     cheerful mock would let that guard rot).
//   - A turn this process never began. On a thread it STARTED that is
//     nonsense — it ran the whole history — and staying an error keeps a
//     tripwire a blanket "believe every anchor" would have thrown away.
//     On a thread it RESUMED it is ignorance: the turns are in a rollout
//     the mock does not keep, and EVERY real rollback arrives this way.
func (a *codexAdapter) anchorIsCuttable(turnID string) bool {
	if turnID == "" {
		return false
	}
	if began, finished := a.e.turnStatus(turnID); began {
		return finished
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resumedThread
}

func (a *codexAdapter) currentHistoryMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.historyMode == "" {
		return legacyHistoryMode
	}
	return a.historyMode
}

// revertThread answers `thread/revert`: the cut that keeps the thread's
// identity.
//
// The response shape is upstream's ThreadRevertResponse — the thread with
// its turns array EMPTY (upstream documents the re-hydration as a
// separate `thread/turns/list` call) plus the two backwards cursors —
// followed immediately, on the same connection, by the `thread/reverted`
// notification the session waits on to confirm the cut.
//
// A scenario-declared `thread/revert` response still wins, matching
// forkThread: a test that wants the failure path writes it out.
func (a *codexAdapter) revertThread(id json.RawMessage, params json.RawMessage) {
	threadID := a.e.currentVars()["THREAD_ID"]
	beforeTurnID := readParamString(params, "beforeTurnId")
	// Reported before the answer so a test can assert WHICH cut the app
	// chose without racing the response, the same ordering thread/start's
	// session-config report uses.
	a.e.rep.report(control.Report{
		Kind:       control.ReportHistoryCut,
		Detail:     "thread/revert",
		Input:      beforeTurnID,
		SessionRef: threadID,
	})
	if _, scripted := a.responses["thread/revert"]; scripted {
		a.respond(id, "thread/revert", a.e.currentVars())
		return
	}
	// The checks run in upstream's own order — params, then the thread,
	// then its history contract, then the anchor — so a client that trips
	// two of them sees the same one a real app-server would report.
	if beforeTurnID == "" {
		// Upstream's params type requires it; an absent field fails
		// deserialisation before the handler runs at all.
		a.writeRPCError(id, -32602, "thread/revert: beforeTurnId is required")
		return
	}
	if requested := readParamString(params, "threadId"); requested != "" && requested != threadID {
		a.writeRPCError(id, -32600, fmt.Sprintf("thread/revert: thread %q is not loaded", requested))
		return
	}
	if a.currentHistoryMode() != paginatedHistoryMode {
		// -32600 invalid_request, verbatim wording: upstream raises this
		// BEFORE it touches the thread, which is the property that makes
		// AO's fall-back-to-fork safe rather than a guess.
		a.writeRPCError(id, -32600, revertRefusalMessage)
		return
	}
	if !a.anchorIsCuttable(beforeTurnID) {
		a.writeRPCError(id, -32602, fmt.Sprintf(
			"thread/revert: beforeTurnId %q is unknown or names the in-progress turn", beforeTurnID))
		return
	}
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"thread": map[string]any{
				"id":          threadID,
				"historyMode": paginatedHistoryMode,
				"turns":       []any{},
			},
			"turnsBackwardsCursor": nil,
			"itemsBackwardsCursor": nil,
		},
	}), 0, 0)
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  "thread/reverted",
		"params":  map[string]any{"threadId": threadID},
	}), 0, 0)
}

// codexThreadStatePath locates the mock's thread-store entry for a thread,
// or reports that this invocation has no durable home (standalone runs and
// unit tests, which must never discover or mutate a real provider home).
func codexThreadStatePath(threadID string) (string, bool) {
	home := strings.TrimSpace(os.Getenv(control.EnvTranscriptHome))
	if home == "" || threadID == "" {
		return "", false
	}
	if filepath.Base(threadID) != threadID ||
		strings.ContainsAny(threadID, `/\`) ||
		strings.ContainsRune(threadID, '\x00') ||
		threadID == "." || threadID == ".." {
		log.Fatalf("codex: unsafe thread id %q", threadID)
	}
	return filepath.Join(home, ".codex-mock-threads", threadID+".json"), true
}

func (a *codexAdapter) persistHistoryMode(threadID, mode string) {
	path, ok := codexThreadStatePath(threadID)
	if !ok {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Fatalf("codex: create mock thread store: %v", err)
	}
	body := mustJSON(map[string]any{"historyMode": mode}) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		log.Fatalf("codex: write mock thread state: %v", err)
	}
}

func (a *codexAdapter) loadHistoryMode(threadID string) string {
	path, ok := codexThreadStatePath(threadID)
	if !ok {
		return legacyHistoryMode
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("codex: read mock thread state: %v", err)
		}
		return legacyHistoryMode
	}
	var state struct {
		HistoryMode string `json:"historyMode"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || strings.TrimSpace(state.HistoryMode) == "" {
		return legacyHistoryMode
	}
	return state.HistoryMode
}

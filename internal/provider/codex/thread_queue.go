package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// The provider-native user-message queue (`thread/queue/*`, codex >= 0.148).
//
// Upstream describes it as "persist a user turn for automatic FIFO submission
// when the thread next becomes idle" (codex-rs/app-server/README.md). Two
// facts decide everything in this file:
//
//  1. **Dispatch is automatic.** `QueuedItemService` is a
//     `ThreadLifecycleContributor`: its `on_thread_idle` hook drains the head
//     of the queue into `start_turn_if_idle` and deletes the row it started
//     (codex-rs/ext/queue/src/service.rs `dispatch_if_idle`). `thread/queue/start`
//     exists only to start a SELECTED submission early; a client that also
//     called it would race the automatic drain and could double-send. AO never
//     calls it — that is the whole "mutually exclusive with AO's flushqueue"
//     rule, stated on the wire side.
//  2. **The dispatched turn has no `turn/start` of ours.** Because the
//     app-server starts it, `turn/started` arrives unclaimed and would be
//     classified as an externally injected turn (external_turns.go) — which
//     would mark the user's OWN message as "queued from outside Agent
//     Overflow". `noteSelfQueuedSubmission` is what prevents that: every
//     submission AO adds leaves a FIFO claim that the next unclaimed
//     `turn/started` consumes.
//
// Everything here is `#[experimental]` upstream, so it rides the
// `capabilities.experimentalApi` flag that `codexInitializeParams` already
// sets for every AO session (same flag the TUI sets at
// codex-rs/tui/src/lib.rs:426,583, and the same one the background-terminal
// RPCs ride).

// threadQueueMinimumCodexVersion is the floor for the whole `thread/queue/*`
// family. The methods do not exist below it, so a call to a 0.147 app-server
// is a hard `method not found`, not a graceful degradation — exactly the
// shape `appServerAtLeast` exists to gate. An unknown version reads as too
// old and AO keeps its own in-process queue.
const threadQueueMinimumCodexVersion = "0.148.0"

// maxSelfQueuedClaims bounds the ledger of claims for submissions AO itself
// put in the provider's queue.
//
// It is NOT the same bound as AO's own composer queue (app_flush_queue.go
// queueMaxLength, 64). That cap counts messages still waiting in AO's hands;
// a message handed to `thread/queue/add` leaves it and leaves a claim behind
// instead, and the claim only clears when the provider dispatches the row —
// which cannot happen until the running turn ends. A long turn can therefore
// accumulate more provider-side claims than AO's own queue would ever hold at
// once, which is why this number is its own and larger than 64.
//
// Reaching it FAILS the add (noteSelfQueuedSubmission → QueueAdd). Silently
// dropping the claim instead would let that message's dispatched turn read as
// externally injected: the echo is stamped `external-queue`, triage refuses to
// pop the pending send, and the user's own prompt lands as a second row while
// the original stays stranded. A refused add leaves the message in AO's queue
// with a visible error, which is recoverable; the other outcome corrupts the
// transcript.
const maxSelfQueuedClaims = 256

// maxReportedForeignSubmissions bounds the foreign-submission dedupe set,
// which is fed straight from the wire (every `thread/queue/changed` re-lists)
// and so needs a bound of its own. Unlike the claim ledger, overrunning this
// one costs a duplicate notice, never a message.
const maxReportedForeignSubmissions = 64

// queueListPageCap bounds a single `thread/queue/list` walk. The response is
// paginated (`nextCursor`) and the page size is server-clamped, so a
// pathological queue cannot be read in one frame; this caps the number of
// pages AO will follow before it reports what it has.
const queueListPageCap = 8

// ErrThreadQueueUnsupported is returned by every queue method when the
// connected app-server predates the family. Callers fall back to AO's own
// queue rather than surfacing an error to the user: the capability is a
// property of the binary, not of the request.
var ErrThreadQueueUnsupported = errors.New("codex: thread/queue is unavailable on this app-server")

// IsThreadQueueUnsupported reports the version-gate refusal. Exported so the
// app layer's dispatcher can branch without importing the sentinel by value.
func IsThreadQueueUnsupported(err error) bool {
	return errors.Is(err, ErrThreadQueueUnsupported)
}

// ErrThreadQueueListIncomplete says a `thread/queue/list` walk stopped before
// the server ran out of pages — the page cap was reached, or the server
// echoed a cursor it had already given.
//
// It rides WITH the rows collected so far, because a prefix is still useful
// to a caller that only wants to recognise rows it can see. What a prefix must
// never do is stand in for the whole queue: a purge would report a complete
// job over a partial list and leave a rolled-back message armed to re-run, and
// a resume-side re-arm would silently omit the rows past the cut and stamp the
// user's own prompts as externally injected. Both of those callers treat this
// as a failure, never as an empty tail.
var ErrThreadQueueListIncomplete = errors.New("codex: thread/queue/list did not reach the end of the queue")

// IsThreadQueueListIncomplete reports the partial-list condition above.
func IsThreadQueueListIncomplete(err error) bool {
	return errors.Is(err, ErrThreadQueueListIncomplete)
}

// ErrThreadQueueListMalformed says a `thread/queue/list` page carried an
// element this build could not read — a wrong-typed field, a missing one, or a
// server-assigned id that came back empty.
//
// It rides with the prefix for the same reason ErrThreadQueueListIncomplete
// does, and it exists for the same reason: a malformed element decoded to an
// EMPTY submission reads as an absent row, and absence is what the two
// recovery callers act on. The resume reconcile would return an unproven row
// to the composer while codex still holds it — a duplicate the moment the user
// re-sends — and the purge would skip the empty id and let the rollback
// truncate history over a submission that is still armed. Upstream's own
// `QueuedSubmission` has three required, non-`Option` fields
// (rust-v0.149.0 codex-rs/app-server-protocol/src/protocol/v2/thread.rs:869),
// so an element that will not decode is a wire fault, never a short row.
var ErrThreadQueueListMalformed = errors.New("codex: thread/queue/list returned a submission this build could not read")

// IsThreadQueueListMalformed reports the unreadable-element condition above.
func IsThreadQueueListMalformed(err error) bool {
	return errors.Is(err, ErrThreadQueueListMalformed)
}

// queueListPrefixIsUsable reports whether a QueueList error still leaves the
// rows it returned worth scanning. Both prefix-bearing sentinels qualify;
// every other shape (a transport failure, a refusal, a body that would not
// decode at all) leaves nothing behind to look at.
func queueListPrefixIsUsable(err error) bool {
	return IsThreadQueueListIncomplete(err) || IsThreadQueueListMalformed(err)
}

// QueuedSubmission is one user message waiting in the PROVIDER's queue.
//
// `ClientUserMessageID` is the correlation handle: `ThreadQueueAddParams`
// requires it (non-`Option` upstream), it survives an update unchanged
// (`QueuedItemService::update` re-reads the stored client id rather than
// taking the caller's), and the dispatched turn's `userMessage` ThreadItem
// echoes it back as `clientId`. AO fills it with the optimistic row id the
// dispatcher just allocated, so a listing can say which entries are AO's own.
type QueuedSubmission struct {
	ID                  string
	Text                string
	ClientUserMessageID string
}

// ThreadQueueNative reports whether this session hands mid-turn user messages
// to the provider's own queue instead of holding them in AO's flushqueue.
//
// Decided ONCE, at handshake time (`recordThreadQueueSupport`), and never
// re-evaluated: the two queues must be mutually exclusive for the life of the
// session, because a decision that flipped mid-session could leave the same
// message in both (double send) or in neither (silent drop). The app layer
// reads this at dispatch to pick the verb.
func (s *Session) ThreadQueueNative() bool { return s.threadQueueNative.Load() }

// recordThreadQueueSupport freezes the queue decision for this session.
// Called exactly once, straight after recordAppServerVersion, so every later
// reader sees a value that cannot change under it.
func (s *Session) recordThreadQueueSupport() {
	s.threadQueueNative.Store(s.appServerAtLeast(threadQueueMinimumCodexVersion))
}

// IsAmbiguousQueueAddTimeout reports the `thread/queue/add` analog of
// IsAmbiguousSteerTimeout: the request reached the app-server but its ack
// never came back, so the row may already be persisted — and, because
// `enqueue` calls `wake_if_loaded`, may already have been dispatched.
//
// Callers must NOT re-send the content on this error. `thread/queue/add` has
// no idempotency key upstream (`QueuedItemService::enqueue` appends
// unconditionally), so a retry is a second row and a second turn. The queue is
// listable, so the right answer is to ASK — see the flush dispatcher's
// resolveAmbiguousQueueAdd.
func IsAmbiguousQueueAddTimeout(err error) bool {
	return IsRequestTimeout(err, "thread/queue/add")
}

// QueueAdd persists one user message in the provider's queue
// (`thread/queue/add`).
//
// It is NOT a send: nothing runs until the thread next goes idle, at which
// point the app-server starts the turn itself. The claim registered on
// success is what keeps that turn attributed to this app.
//
// clientUserMessageID must be AO's optimistic row id for the message. Upstream
// requires the field and mints a uuid when a producer omits it, so passing an
// empty string would silently give up the correlation rather than fail.
//
// **The safety axes are asserted first.** `ThreadQueueAddParams` is
// `{threadId, input, clientUserMessageId}` and nothing else
// (codex-rs/app-server-protocol/src/protocol/v2/thread.rs:878), and the drain
// starts the turn with `TurnInputRequest::new(input)` — a
// `ThreadSettingsOverrides::default()`, i.e. NO per-turn overrides
// (codex-rs/ext/queue/src/service.rs:433). A queued turn therefore runs under
// whatever the THREAD is configured to do. AO asserts approvalPolicy /
// sandboxPolicy / approvalsReviewer per turn on `turn/start` and otherwise
// keeps a runtime-mode change in memory (live_update.go ApplyLiveUpdate), so
// without the push below a user who tightens the mode while a turn is running
// — exactly when messages get queued — would have the queued turn execute
// under the older, looser policy. pushQueueTurnConfig closes that: the thread
// holds the same config a turn/start would have asserted before the row
// exists. A failed push fails the add; nothing is queued under an unasserted
// policy.
func (s *Session) QueueAdd(
	ctx context.Context, content string, opts provider.SendOptions, clientUserMessageID string,
) (QueuedSubmission, error) {
	if !s.ThreadQueueNative() {
		return QueuedSubmission{}, ErrThreadQueueUnsupported
	}
	if strings.TrimSpace(clientUserMessageID) == "" {
		return QueuedSubmission{}, fmt.Errorf("codex: thread/queue/add: empty client user message id")
	}
	input, err := buildTurnInput(content, opts.Attachments)
	if err != nil {
		return QueuedSubmission{}, fmt.Errorf("codex: thread/queue/add: %w", err)
	}
	if err := s.pushQueueTurnConfig(ctx); err != nil {
		return QueuedSubmission{}, fmt.Errorf("codex: thread/queue/add: %w", err)
	}
	// Claim BEFORE the write, for the same reason Send claims before
	// `turn/start`: an idle thread dispatches the row inside `enqueue`
	// itself (`wake_if_loaded` → the idle lifecycle hook), so `turn/started`
	// can reach the read loop before this request's response does.
	//
	// A claim that cannot be recorded FAILS the add rather than writing
	// anyway. The claim is not bookkeeping — it is the only thing that keeps
	// the dispatched turn attributed to this app, and a submission written
	// without one comes back as the user's own prompt labelled "queued from
	// outside Agent Overflow", with a duplicate row beside the original.
	// See maxSelfQueuedClaims.
	if !s.noteSelfQueuedSubmission(clientUserMessageID) {
		return QueuedSubmission{}, fmt.Errorf(
			"codex: thread/queue/add: %d messages are already waiting in this thread's Codex queue; let some of them run first",
			maxSelfQueuedClaims)
	}
	resp, err := s.sendRequest(ctx, "thread/queue/add", map[string]any{
		"threadId":            s.rootThreadID(),
		"input":               input,
		"clientUserMessageId": clientUserMessageID,
	})
	if err != nil {
		// A definite failure releases the claim. A TIMEOUT does not — the row
		// may exist and may already have been dispatched, and a released
		// claim would mislabel that turn as externally injected. Same
		// asymmetry as abandonLocalTurnStart.
		if !IsAmbiguousQueueAddTimeout(err) {
			s.abandonSelfQueuedSubmission(clientUserMessageID)
		}
		return QueuedSubmission{}, fmt.Errorf("codex: thread/queue/add: %w", err)
	}
	// A malformed ACK is not a failed add. The write landed — that is what the
	// response is acknowledging — and `thread/queue/add` has no idempotency
	// key, so returning an error here would send the caller down the definite-
	// failure path (claim abandoned, row unwound) over a message the provider
	// is holding and about to run. The claim is keyed by CLIENT id and was
	// made before the write, so it survives without the submission id; what is
	// lost is only the ability to release it by submission id later, which the
	// resume reconcile and the purge both re-derive. So: log, do not fail.
	submission, parseErr := parseQueuedSubmissionObject(readNestedObject(resp, "queuedSubmission"))
	if parseErr != nil {
		log.Printf("codex: thread/queue/add on thread %s was acked with an unreadable queuedSubmission (%v); the message is queued, but this session cannot name its submission id",
			s.threadID, parseErr)
	}
	s.bindSelfQueuedSubmission(clientUserMessageID, submission.ID)
	return submission, nil
}

// pushQueueTurnConfig asserts this session's whole turn config onto the thread
// through `thread/settings/update`, so a row about to enter the provider's
// queue cannot run under stale settings.
//
// Every axis is named unconditionally — this is an ASSERTION, not a diff.
// PlanThreadSettingsPush's "only what changed" rule answers a different
// question (what does a live composer change need to land?); here the question
// is "is the thread currently configured the way the turn that runs this row
// must be?", and the only honest answer states all of them.
func (s *Session) pushQueueTurnConfig(ctx context.Context) error {
	if err := s.PushThreadSettings(ctx, queueNativeSettingsPush()); err != nil {
		return err
	}
	s.mu.Lock()
	unsupported := s.settingsUpdateUnsupported
	s.mu.Unlock()
	if unsupported {
		// Unreachable on any binary that has the queue: both families are
		// `#[experimental]` and `thread/settings/update` predates
		// `thread/queue/*`. Refusing rather than queueing anyway is the
		// fail-closed direction — the caller surfaces it and the message
		// stays in AO's hands.
		return fmt.Errorf(
			"%s is unavailable on this app-server, so a queued turn's approval and sandbox policy cannot be asserted",
			threadSettingsUpdateMethod)
	}
	return nil
}

// QueueList reads the provider's queue for this thread, following
// `nextCursor` up to queueListPageCap pages.
//
// The list is the only way to learn queue DEPTH: `thread/queue/changed`
// carries `{threadId}` and nothing else at rust-v0.149.0. It is also how AO
// tells its own pending entries from a foreign producer's.
//
// A walk that ends because it ran out of PAGES rather than out of rows
// returns the prefix plus ErrThreadQueueListIncomplete. Answering "here is
// the queue" with a prefix is the one shape that silently corrupts both
// callers — see that sentinel's doc.
func (s *Session) QueueList(ctx context.Context) ([]QueuedSubmission, error) {
	if !s.ThreadQueueNative() {
		return nil, ErrThreadQueueUnsupported
	}
	var out []QueuedSubmission
	cursor := ""
	for page := 0; page < queueListPageCap; page++ {
		params := map[string]any{"threadId": s.rootThreadID()}
		if cursor != "" {
			params["cursor"] = cursor
		}
		resp, err := s.sendRequest(ctx, "thread/queue/list", params)
		if err != nil {
			return out, fmt.Errorf("codex: thread/queue/list: %w", err)
		}
		var body struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp, &body); err != nil {
			return out, fmt.Errorf("codex: thread/queue/list: decode response: %w", err)
		}
		for _, raw := range body.Data {
			submission, parseErr := parseQueuedSubmission(raw)
			if parseErr != nil {
				// Stop the walk. What is in hand is a prefix, exactly as it is
				// for a repeated cursor, and paging further would only spend
				// RPCs on an answer that can no longer be complete.
				return out, fmt.Errorf("%w: %v (after %d row(s))",
					ErrThreadQueueListMalformed, parseErr, len(out))
			}
			out = append(out, submission)
		}
		if body.NextCursor == nil || strings.TrimSpace(*body.NextCursor) == "" {
			return out, nil
		}
		// A server that echoes the same cursor forever would spin this loop;
		// the page cap bounds it, but stopping on a repeat is cheaper and
		// says why. Either way the server still had pages to give, so what
		// is in hand is a PREFIX and says so.
		if *body.NextCursor == cursor {
			return out, fmt.Errorf("%w: the app-server repeated cursor %q after %d row(s)",
				ErrThreadQueueListIncomplete, cursor, len(out))
		}
		cursor = *body.NextCursor
	}
	return out, fmt.Errorf("%w: stopped at the %d-page cap with %d row(s) read",
		ErrThreadQueueListIncomplete, queueListPageCap, len(out))
}

// QueueDelete removes a queued submission (`thread/queue/delete`).
//
// `deleted: false` means "matched nothing" — the row was already dispatched or
// already gone — which is a state, not an error, exactly like
// TerminateBackgroundTerminal's `terminated: false`.
//
// **`deleted` is REQUIRED, and an absent one is a hard error.** Upstream types
// the response as `ThreadQueueDeleteResponse { pub deleted: bool }`
// (rust-v0.149.0 codex-rs/app-server-protocol/src/protocol/v2/thread.rs:940):
// non-`Option`, no `#[serde(default)]`, so upstream's own deserializer refuses
// a body without the key. Decoding it into a plain `bool` would turn any
// schema drift — a rename, a nested envelope, an explicit `null` — into a
// benign-looking `false`, and the two things that read that answer both treat
// `false` as "already gone": the claim ledger keeps the claim (correct only if
// the row really did dispatch) and PurgeQueue counts the row as accounted for,
// so the rollback truncates history over a submission that may still be armed.
// A pointer makes absence and null distinguishable from a genuine `false`.
//
// The self-claim is released ONLY on a real delete. `deleted: false` most
// often means the drain already started the row's turn, and that turn's
// `turn/started` has not necessarily reached the read loop yet — releasing
// the claim there would leave AO's own message classified `external-queue`.
func (s *Session) QueueDelete(ctx context.Context, submissionID string) (bool, error) {
	if !s.ThreadQueueNative() {
		return false, ErrThreadQueueUnsupported
	}
	if strings.TrimSpace(submissionID) == "" {
		return false, fmt.Errorf("codex: thread/queue/delete: empty submission id")
	}
	resp, err := s.sendRequest(ctx, "thread/queue/delete", map[string]any{
		"threadId":           s.rootThreadID(),
		"queuedSubmissionId": submissionID,
	})
	if err != nil {
		return false, fmt.Errorf("codex: thread/queue/delete: %w", err)
	}
	var body struct {
		Deleted *bool `json:"deleted"`
	}
	if err := json.Unmarshal(resp, &body); err != nil {
		return false, fmt.Errorf("codex: thread/queue/delete: decode response: %w", err)
	}
	if body.Deleted == nil {
		return false, fmt.Errorf(
			"codex: thread/queue/delete: the app-server answered submission %s without a `deleted` field, so whether the row is still queued is unknown",
			submissionID)
	}
	if *body.Deleted {
		s.forgetSelfQueuedSubmissionID(submissionID)
	}
	return *body.Deleted, nil
}

// QueuePurge is what one PurgeQueue call ESTABLISHED about the provider's
// queue, as distinct from whether the call errored.
//
// `Deleted` names every submission the app-server confirmed it removed, in the
// order they went. It is not a statistic: a purge that then fails halfway
// leaves the rollback refused and history untouched, and the rows already gone
// from the provider's queue are messages that nothing will ever run again
// unless the caller puts them somewhere. Only the caller can do that — it owns
// the store rows those client ids name — so this has to report exactly which
// rows went, not how many. See app_flush_queue_provider.go's
// restorePurgedProviderQueueRows.
//
// `Foreign` counts the `Deleted` rows this SESSION had no claim for. It is a
// log figure, deliberately not an ownership verdict: the session's claim
// ledger is in-memory and rebuilt from the store, so the app layer re-derives
// ownership from its own rows rather than trusting this count.
type QueuePurge struct {
	Deleted []QueuedSubmission
	Foreign int
}

// PurgeQueue deletes every submission currently in the provider's queue for
// this thread and reports exactly which ones it removed.
//
// It exists for conversation rollback. AO's own flushqueue is cleared in
// process (`clearFlushDispatchForRollback`), but a message already handed to
// `thread/queue/add` lives in codex's SQLite: it survives the session stop,
// and `QueuedItemService::on_thread_idle` dispatches it on the next resume —
// re-running, onto a thread the user just truncated, a message they rolled
// back.
//
// Foreign rows are deleted too, and named in the log. A row AO did not author
// would re-run onto the truncated thread exactly like AO's own, and there is
// no way to leave it queued that does not carry that hazard; what AO can do
// is say out loud that it dropped somebody else's message. AO cannot put a
// foreign row back — there is no `thread/queue/add` that preserves another
// producer's authorship, and re-adding it would announce it as this app's — so
// a dropped foreign row is reported, never restored.
//
// Best-effort per row: one refusal does not abandon the rest, because a
// partially purged queue is strictly better than an untouched one. What makes
// that safe is the report: with A deleted and B refused the caller aborts the
// rollback AND takes A back, so an abandoned rollback still loses nothing of
// the user's own.
func (s *Session) PurgeQueue(ctx context.Context) (QueuePurge, error) {
	if !s.ThreadQueueNative() {
		return QueuePurge{}, ErrThreadQueueUnsupported
	}
	submissions, listErr := s.QueueList(ctx)
	if listErr != nil && len(submissions) == 0 {
		return QueuePurge{}, listErr
	}
	owned := s.selfQueuedClientIDs()
	purge := QueuePurge{}
	var failures []error
	if listErr != nil {
		failures = append(failures, listErr)
	}
	for _, submission := range submissions {
		if submission.ID == "" {
			// Unreachable through QueueList (parseQueuedSubmission refuses an
			// element with no id), and a FAILURE rather than a skip if it ever
			// becomes reachable: a row with no delete handle is a row still in
			// the queue, and skipping it silently is what would let the caller
			// truncate history over it.
			failures = append(failures, fmt.Errorf(
				"codex: thread/queue: a queued submission on thread %s has no id, so it cannot be deleted", s.threadID))
			continue
		}
		removed, deleteErr := s.QueueDelete(ctx, submission.ID)
		if deleteErr != nil {
			failures = append(failures, deleteErr)
			continue
		}
		if !removed {
			continue
		}
		purge.Deleted = append(purge.Deleted, submission)
		if _, mine := owned[submission.ClientUserMessageID]; !mine {
			purge.Foreign++
			// Logged AFTER the delete landed: before it, this line would name
			// a row that may still be sitting in the queue.
			log.Printf(
				"codex: rollback dropped queued submission %s on thread %s that Agent Overflow did not add and cannot restore (clientUserMessageId=%q)",
				submission.ID, s.threadID, submission.ClientUserMessageID)
		}
		// Release the claim by CLIENT id as well. QueueDelete releases by
		// SUBMISSION id, which only finds a claim this session bound at its
		// own add — a claim rebuilt by RearmSelfQueuedClaims after a session
		// death carries no submission id at all, so without this the claim for
		// a row that is now gone would outlive it and defer every later
		// `turn/started` on this connection.
		s.abandonSelfQueuedSubmission(submission.ClientUserMessageID)
	}
	if len(failures) == 0 {
		// The claim ledger is meaningless once the queue is gone: every row it
		// pointed at has been deleted, and a stale claim would absorb an
		// unrelated later turn. QueueDelete already released each row it
		// removed; this is the sweep for rows that answered `deleted: false`.
		//
		// Scoped to a COMPLETE purge on purpose. A partial one leaves rows in
		// the provider's queue, its caller refuses the rollback rather than
		// truncating over them, and the session keeps running — so a cleared
		// ledger would mean the next dispatch of one of those surviving rows
		// gets stamped `external-queue` and reported to the user as somebody
		// else's message.
		s.forgetSelfQueuedClaims()
	}
	return purge, errors.Join(failures...)
}

// RearmSelfQueuedClaims restores the claim ledger from rows that survived a
// session death, so a submission AO queued BEFORE the process went away is
// still recognised as this app's when the new connection's idle hook
// dispatches it.
//
// The ledger is in-memory by design (a claim is about a turn this connection
// will see), which is exactly why it has to be rebuilt: without this the
// resumed session sees a `turn/started` it never asked for, finds no claim,
// and stamps the user's own message `external-queue`.
//
// Ownership is decided by the CALLER, not here: `clientUserMessageId` is AO's
// own row id and only the app layer can say which ids are its. Existing
// claims are preserved and duplicates are ignored, so calling this twice is
// harmless.
func (s *Session) RearmSelfQueuedClaims(clientIDs []string) int {
	if len(clientIDs) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selfQueuedSubmissions == nil {
		s.selfQueuedSubmissions = make(map[string]string, len(clientIDs))
	}
	rearmed := 0
	for _, clientID := range clientIDs {
		if strings.TrimSpace(clientID) == "" {
			continue
		}
		if _, already := s.selfQueuedSubmissions[clientID]; already {
			continue
		}
		if len(s.selfQueuedSubmissions) >= maxSelfQueuedClaims {
			log.Printf("codex: self-queued claim re-arm stopped at cap (%d) on thread %s",
				maxSelfQueuedClaims, s.threadID)
			break
		}
		s.selfQueuedSubmissions[clientID] = ""
		rearmed++
	}
	return rearmed
}

// parseQueuedSubmissionObject is parseQueuedSubmission for a value already
// navigated to as an object (the single-submission responses).
func parseQueuedSubmissionObject(obj map[string]json.RawMessage) (QueuedSubmission, error) {
	if obj == nil {
		return QueuedSubmission{}, fmt.Errorf("no queuedSubmission object in the response")
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return QueuedSubmission{}, fmt.Errorf("re-encode queuedSubmission: %w", err)
	}
	return parseQueuedSubmission(raw)
}

// parseQueuedSubmission projects the wire `QueuedSubmission` onto AO's shape.
// The input vec is flattened to its text in wire order — the same projection
// `codexInputText` does on the mock side — because a queued row's images are
// snapshotted by the server and AO has no use for the paths.
//
// It REPORTS a failure rather than returning a zero value. Upstream's
// `QueuedSubmission` is `{id: String, input: Vec<UserInput>,
// client_user_message_id: String}`, all three required and non-`Option`
// (rust-v0.149.0 codex-rs/app-server-protocol/src/protocol/v2/thread.rs:869),
// so nothing on the wire produces an element with no id — and an empty
// submission is indistinguishable from an ABSENT one to both callers that act
// on absence. See ErrThreadQueueListMalformed.
func parseQueuedSubmission(raw json.RawMessage) (QueuedSubmission, error) {
	var body struct {
		ID                  string `json:"id"`
		ClientUserMessageID string `json:"clientUserMessageId"`
		Input               []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}
	if len(raw) == 0 {
		return QueuedSubmission{}, fmt.Errorf("empty submission element")
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return QueuedSubmission{}, fmt.Errorf("decode submission: %w", err)
	}
	if strings.TrimSpace(body.ID) == "" {
		// The id is server-assigned and is the ONLY handle a delete takes, so
		// a row without one cannot be purged. Reporting it is what keeps a
		// rollback from truncating over it.
		return QueuedSubmission{}, fmt.Errorf("submission has no id")
	}
	var text strings.Builder
	for _, item := range body.Input {
		if item.Type == "text" {
			text.WriteString(item.Text)
		}
	}
	return QueuedSubmission{
		ID:                  body.ID,
		Text:                text.String(),
		ClientUserMessageID: body.ClientUserMessageID,
	}, nil
}

// --- self-queued claims -------------------------------------------------
//
// A submission AO added is one turn this session will later see start without
// having asked for it. The claims below are what let adoptTurnStart tell that
// turn from a genuinely foreign one. They are held under `mu` alongside the
// rest of the turn-origin bookkeeping so a single lock orders both.
//
// The ledger is a MAP keyed by client id, not a FIFO, because nothing about
// it is ordered: every read is either "is this exact client id mine" or "how
// many are outstanding". The provider does drain its queue in order, but a
// foreign producer's rows share that FIFO, so position was never usable as
// the key — see takeSelfQueuedClaimForClientLocked. A map is also what makes
// the cap honest: an entry can only be displaced by its own release, never by
// a newer claim needing the room.

func (s *Session) noteSelfQueuedSubmission(clientID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selfQueuedSubmissions == nil {
		s.selfQueuedSubmissions = make(map[string]string, 2)
	}
	if _, held := s.selfQueuedSubmissions[clientID]; held {
		// Already claimed (a re-arm ran first, or a caller retried). Nothing
		// to add and nothing to refuse.
		return true
	}
	if len(s.selfQueuedSubmissions) >= maxSelfQueuedClaims {
		log.Printf("codex: refusing a self-queued submission claim at cap (%d) on thread %s",
			maxSelfQueuedClaims, s.threadID)
		return false
	}
	s.selfQueuedSubmissions[clientID] = ""
	return true
}

// bindSelfQueuedSubmission attaches the server-assigned submission id to a
// claim already made by client id, so a later delete can release it.
func (s *Session) bindSelfQueuedSubmission(clientID, submissionID string) {
	if submissionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, held := s.selfQueuedSubmissions[clientID]; held && existing == "" {
		s.selfQueuedSubmissions[clientID] = submissionID
	}
}

func (s *Session) abandonSelfQueuedSubmission(clientID string) {
	s.mu.Lock()
	s.dropSelfQueuedClaimLocked(clientID)
	s.mu.Unlock()
}

// dropSelfQueuedClaimLocked removes the claim for clientID and reports
// whether one was there. Caller holds mu.
func (s *Session) dropSelfQueuedClaimLocked(clientID string) bool {
	if _, held := s.selfQueuedSubmissions[clientID]; !held {
		return false
	}
	delete(s.selfQueuedSubmissions, clientID)
	return true
}

func (s *Session) forgetSelfQueuedSubmissionID(submissionID string) {
	if submissionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for clientID, bound := range s.selfQueuedSubmissions {
		if bound == submissionID {
			delete(s.selfQueuedSubmissions, clientID)
			return
		}
	}
}

// takeSelfQueuedClaimForClientLocked consumes the claim whose client id the
// dispatched turn echoed. Caller holds mu.
//
// Keyed by client id, never by position. The provider FIFO can hold rows from
// other producers (`codex queue --thread …`) interleaved with AO's, and
// upstream drains it strictly in order — so a foreign row AHEAD of AO's would
// pop AO's claim, and the foreign turn's user echo would render as the user's
// own message in this app.
func (s *Session) takeSelfQueuedClaimForClientLocked(clientID string) bool {
	if clientID == "" {
		return false
	}
	return s.dropSelfQueuedClaimLocked(clientID)
}

// hasSelfQueuedClaimsLocked reports whether any submission AO added is still
// waiting to be dispatched. Caller holds mu.
func (s *Session) hasSelfQueuedClaimsLocked() bool {
	return len(s.selfQueuedSubmissions) > 0
}

// forgetSelfQueuedClaims drops the whole ledger. Used when the queue itself is
// known to be gone (PurgeQueue).
func (s *Session) forgetSelfQueuedClaims() {
	s.mu.Lock()
	clear(s.selfQueuedSubmissions)
	s.mu.Unlock()
}

// selfQueuedClientIDs snapshots the client ids AO currently owns in the
// provider queue. Used by the change reconciler to tell its own entries from
// a foreign producer's.
func (s *Session) selfQueuedClientIDs() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]struct{}, len(s.selfQueuedSubmissions))
	for clientID := range s.selfQueuedSubmissions {
		out[clientID] = struct{}{}
	}
	return out
}

// --- thread/queue/changed reconciliation --------------------------------

// reconcileThreadQueueChange decides what a `thread/queue/changed` means.
//
// Without the provider queue adopted, the notification can ONLY be a foreign
// producer (`codex queue --thread …`), so the classifier's notice stands as
// written and this is a pass-through.
//
// With it adopted, AO's own add / update / delete / reorder each raise one
// `changed`, and so does every automatic dispatch (the drain deletes the row
// it started). Emitting the notice on those would tell the user their own
// message came from outside the app. So the notice becomes evidence-driven
// rather than event-driven: the immediate event is dropped and a bounded
// `thread/queue/list` decides, raising the notice only for a submission whose
// `clientUserMessageId` AO does not own.
//
// The list runs on its own goroutine because this is the read loop: a
// synchronous request here would deadlock on its own response.
func (s *Session) reconcileThreadQueueChange(events []provider.ProviderEvent) []provider.ProviderEvent {
	if !s.ThreadQueueNative() {
		return events
	}
	s.startQueueReconcile()
	return nil
}

// startQueueReconcile single-flights the list walk.
//
// AO's own add and delete each raise a `thread/queue/changed`, and so does
// every automatic dispatch (the drain deletes the row it started), so an
// N-message queue produces ~2N notifications. One goroutine per notification,
// each walking up to queueListPageCap paginated list RPCs, is a burst the
// answer does not need: the queue's state at the END of the burst is the only
// state worth reporting. A change that lands while a walk is in flight sets
// the dirty flag and the same goroutine walks once more, so the last
// notification is never the dropped one.
func (s *Session) startQueueReconcile() {
	s.mu.Lock()
	if s.queueListInflight {
		s.queueListDirty = true
		s.mu.Unlock()
		return
	}
	s.queueListInflight = true
	s.mu.Unlock()

	go func() {
		for {
			s.listQueueForForeignSubmissions()
			s.mu.Lock()
			if !s.queueListDirty {
				s.queueListInflight = false
				s.mu.Unlock()
				return
			}
			s.queueListDirty = false
			s.mu.Unlock()
		}
	}()
}

// listQueueForForeignSubmissions raises one external-queue notice per foreign
// submission id it has never reported.
//
// A foreign row that is added and dispatched inside the poll window is missed
// here — the queue is empty by the time the list answers. That is deliberate:
// the injected turn itself still carries `origin=external-queue` from
// external_turns.go, which is the marking that actually protects the
// transcript. A notice AO cannot substantiate is worse than a missing one.
func (s *Session) listQueueForForeignSubmissions() {
	// Derived from the SESSION's context, not Background: this runs off the
	// read loop on a goroutine nobody joins, and a teardown mid-walk must
	// cancel it rather than hold the process for the request timeout.
	ctx, cancel := context.WithTimeout(s.ctx, defaultRequestTimeout)
	defer cancel()
	submissions, err := s.QueueList(ctx)
	if err != nil {
		if !s.closing.Load() {
			log.Printf("codex: thread/queue/list after change on %s: %v", s.threadID, err)
		}
		// A PREFIX is still worth scanning here, unlike in the purge and the
		// resume re-arm: this walk only ever ADDS notices for rows it can
		// see, and the dedupe makes a later walk that reaches further
		// idempotent. Every other error shape leaves nothing to look at.
		if !queueListPrefixIsUsable(err) || len(submissions) == 0 {
			return
		}
	}
	owned := s.selfQueuedClientIDs()
	for _, submission := range submissions {
		if _, mine := owned[submission.ClientUserMessageID]; mine {
			continue
		}
		if !s.markForeignSubmissionReported(submission.ID) {
			continue
		}
		s.emitEvent(provider.ProviderEvent{
			Kind:     provider.EventNotification,
			ThreadID: s.threadID,
			Content:  externalQueueNoticeText,
			Meta: mergeMetaKeys(nil, map[string]any{
				"kind":   "external_queue",
				"title":  externalQueueNoticeText,
				"origin": ExternalTurnOriginQueue,
			}),
			Timestamp: time.Now(),
		})
	}
}

// markForeignSubmissionReported returns true the first time a foreign
// submission id is seen. The queue is re-listed on every change, so without
// the dedupe a foreign row that sits behind AO's own entries would raise a
// fresh notice on each of AO's mutations.
func (s *Session) markForeignSubmissionReported(submissionID string) bool {
	if submissionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.reportedForeignSubmissions[submissionID]; seen {
		return false
	}
	if s.reportedForeignSubmissions == nil {
		s.reportedForeignSubmissions = make(map[string]struct{}, 2)
	}
	if len(s.reportedForeignSubmissions) >= maxReportedForeignSubmissions {
		// At the cap, stop growing but keep reporting: a duplicate notice is
		// noise, an unbounded map fed from the wire is a leak.
		return true
	}
	s.reportedForeignSubmissions[submissionID] = struct{}{}
	return true
}

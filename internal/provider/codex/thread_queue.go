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
// **AO reads and deletes; it never adds.** Mid-turn user messages go to
// `turn/steer` on every supported codex, so `thread/queue/add` has no caller
// here and no wrapper. What remains is the surface AO needs for a queue it
// does not participate in:
//
//  1. **Read and delete.** `QueueList` / `QueueDelete` / `PurgeQueue` exist
//     for conversation rollback: a row a FOREIGN producer
//     (`codex queue --thread …`) left in codex's SQLite survives a session
//     stop and `on_thread_idle` dispatches it onto the truncated thread at the
//     next resume.
//  2. **The foreign-submission notice.** `thread/queue/changed` carries
//     `{threadId}` and nothing else, so the depth and the authorship both have
//     to be read back with a list.
//
// `thread/queue/start`, `update` and `reorder` are, as before, never called —
// dispatch is automatic (`QueuedItemService::on_thread_idle` →
// `dispatch_if_idle` → `start_turn_if_idle`), so a client `start` races the
// drain, and AO has no surface that edits or re-orders a message the provider
// already owns. `TestQueueStartIsNeverCalled` is the tripwire.
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

// isThreadQueueUnsupported reports the version-gate refusal without importing
// the sentinel by value. Unexported: AO no longer writes to the provider's
// queue, so every branch on it lives inside this package.
func isThreadQueueUnsupported(err error) bool {
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
// `ClientUserMessageID` is the row's correlation handle: whatever producer
// wrote it supplied one (upstream requires the field and mints a uuid for a
// producer that omits it), it survives an update unchanged
// (`QueuedItemService::update` re-reads the stored client id rather than
// taking the caller's), and the dispatched turn's `userMessage` ThreadItem
// echoes it back as `clientId`. AO never writes rows here, so every id it sees
// belongs to somebody else unless Config.OwnsQueuedClientID says otherwise.
type QueuedSubmission struct {
	ID                  string
	Text                string
	ClientUserMessageID string
}

// ThreadQueueNative reports whether the connected app-server HAS the
// `thread/queue/*` family at all — i.e. whether `list` and `delete` are
// callable on this session.
//
// It stopped being a dispatch decision when AO left the provider's queue: a
// mid-turn message goes to `turn/steer` either way. What it still gates is
// recovery. A rollback has to purge rows a foreign producer left in codex's
// SQLite, and on an app-server that cannot list them there is nothing to
// attempt — which is a state the app layer must be able to see rather than a
// failure to swallow.
//
// Decided ONCE, at handshake time (`recordThreadQueueSupport`), and never
// re-evaluated: a value that could flip mid-session would let one caller
// believe the queue is readable and the next believe it is not, with no way
// to reconcile what either of them concluded.
func (s *Session) ThreadQueueNative() bool { return s.threadQueueNative.Load() }

// recordThreadQueueSupport freezes the queue decision for this session.
// Called exactly once, straight after recordAppServerVersion, so every later
// reader sees a value that cannot change under it.
func (s *Session) recordThreadQueueSupport() {
	s.threadQueueNative.Store(s.appServerAtLeast(threadQueueMinimumCodexVersion))
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
// benign-looking `false` — which the one thing that reads this answer treats
// as "already gone": PurgeQueue counts the row as accounted for, so the
// rollback truncates history over a submission that may still be armed. A
// pointer makes absence and null distinguishable from a genuine `false`.
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
// rows went, not how many. See app_codex_provider_queue.go's
// restorePurgedProviderQueueRows.
//
// `Foreign` counts the `Deleted` rows Config.OwnsQueuedClientID did not claim
// for this app. It is a log figure, deliberately not an ownership verdict: the
// predicate answers from the app layer's own store rows, which is also where
// the caller re-derives ownership when it decides what to restore.
type QueuePurge struct {
	Deleted []QueuedSubmission
	Foreign int
}

// PurgeQueue deletes every submission currently in the provider's queue for
// this thread and reports exactly which ones it removed.
//
// It exists for conversation rollback. AO's own flushqueue is cleared in
// process (`clearFlushDispatchForRollback`), but a row already in codex's
// SQLite survives the session stop and `QueuedItemService::on_thread_idle`
// dispatches it on the next resume — re-running, onto a thread the user just
// truncated, a message they rolled back. Since AO no longer adds rows itself,
// every row this deletes is normally a foreign producer's.
//
// Foreign rows are deleted anyway, and named in the log. A row AO did not
// author carries exactly the same hazard, and there is no way to leave it
// queued that avoids it; what AO can do is say out loud that it dropped
// somebody else's message. It cannot put one back — there is no
// `thread/queue/add` caller here, and re-adding it would announce another
// producer's text as this app's — so a dropped foreign row is reported, never
// restored.
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
		if !s.ownsQueuedClientID(submission.ClientUserMessageID) {
			purge.Foreign++
			// Logged AFTER the delete landed: before it, this line would name
			// a row that may still be sitting in the queue.
			log.Printf(
				"codex: rollback dropped queued submission %s on thread %s that Agent Overflow did not add and cannot restore (clientUserMessageId=%q)",
				submission.ID, s.threadID, submission.ClientUserMessageID)
		}
	}
	return purge, errors.Join(failures...)
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

// --- thread/queue/changed reconciliation --------------------------------

// reconcileThreadQueueChange decides what a `thread/queue/changed` means.
//
// On an app-server with no `thread/queue/list` the notification is all there
// is, so the classifier's own notice stands as written and this is a
// pass-through: it reports that SOMETHING was queued, with no depth and no
// authorship, because nothing can be asked.
//
// Where the family exists the notice becomes evidence-driven instead: the
// immediate event is dropped and a bounded `thread/queue/list` decides, so a
// notice names a submission that was actually there. Ownership is still a
// question — an automatic dispatch deletes the row it started and raises a
// `changed` of its own — and it is answered by Config.OwnsQueuedClientID,
// because only the app layer holds the rows that could claim one.
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
// Every mutation raises a `thread/queue/changed`, and so does every automatic
// dispatch (the drain deletes the row it started), so an N-message queue
// produces ~2N notifications. One goroutine per notification,
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
	for _, submission := range submissions {
		if s.ownsQueuedClientID(submission.ClientUserMessageID) {
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
// the dedupe a row that outlives one change would raise a fresh notice on
// every later one.
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

// ownsQueuedClientID asks the app layer whether a listed submission is one
// this app is responsible for.
//
// It is INJECTED (Config.OwnsQueuedClientID) rather than answered here because
// ownership is a store question: AO's own row ids are deterministic, so the id
// grammar is not a credential — a second Agent Overflow profile against the
// same Codex home mints the same ones, and anything speaking
// `thread/queue/add` may simply supply one. Only the app layer can say which
// ids its own store rows account for.
//
// A nil predicate means EVERY submission is foreign, which is the correct
// default now that this package never writes to the queue: a session that was
// given no way to claim a row has no claim to make.
func (s *Session) ownsQueuedClientID(clientID string) bool {
	if s.ownsQueuedClient == nil || strings.TrimSpace(clientID) == "" {
		return false
	}
	return s.ownsQueuedClient(clientID)
}

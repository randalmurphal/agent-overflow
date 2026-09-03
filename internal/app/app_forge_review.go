package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

const defaultPRUpdateInterval = 45 * time.Second

// maxPRUpdateHandles bounds the outstanding SubscribePRUpdates handles for
// the whole app. Each distinct PR behind them costs a poll goroutine that
// spawns gh/glab every tick, so an unbounded handle map is a
// resource-exhaustion surface reachable by anything that can call the
// binding in a loop — a compromised renderer, a caller that never
// unsubscribes. Review panes are counted in single digits; this is three
// orders of magnitude above any real UI, and matches maxGitWatchHandles.
const maxPRUpdateHandles = 256

// ErrTooManyPRUpdateSubscriptions is returned once the handle cap is hit.
// Typed so the frontend can tell it apart from a PR that failed to fetch —
// this one is never fixed by retrying the same call.
var ErrTooManyPRUpdateSubscriptions = errors.New("pr updates: too many active subscriptions")

// PRUpdateSubscriptionResult is the wire shape returned by
// SubscribePRUpdates. ID is the per-caller handle used to unsubscribe and
// to report visibility; PRKey is the entity key "pr:updated" events are
// addressed by, which the frontend routes on.
//
// Error carries the pump's ACTIVE failure — the same caller-safe summary
// the last "pr:updated" frame carried, never the forge CLI's own stderr.
// The pump dedups identical failures, so a subscriber joining an outage
// gets no frame of its own; without this it would show the pump's stale
// snapshot with no banner until the forge recovered or failed differently.
// Seq stamps the pump state this result was read from, so the caller can
// tell a frame it missed during the join from one already folded in.
type PRUpdateSubscriptionResult struct {
	ID      string                `json:"id"`
	PRKey   string                `json:"prKey"`
	Detail  gitops.PRDetail       `json:"detail"`
	Threads []gitops.ReviewThread `json:"threads"`
	HeadSHA string                `json:"headSHA"`
	Error   string                `json:"error"`
	Seq     uint64                `json:"seq"`
}

// PRUpdatedEvent is the shape pushed on the "pr:updated" channel, once per
// observed change per PR regardless of how many callers subscribed to it.
//
// It carries no subscription id on purpose: a pull request is one entity,
// and addressing its updates per subscriber is what let two panes on the
// same PR hold private copies that disagreed. Error is a fetch failure the
// consumer shows as state on the PR it names — an unreachable forge is a
// fact about the PR, not a log line — and is emitted (and cleared) only on
// change, like the snapshot itself. It is a CALLER-SAFE summary plus a
// correlation id, never the forge CLI's own stderr: see prUpdateErrorMessage.
//
// Seq is the pump-state sequence this frame was stamped with, under the
// same mutex that stored the state. A subscriber compares it against the
// Seq its SubscribePRUpdates result carried: anything at or below is
// already in that snapshot, anything above is a frame the join missed.
type PRUpdatedEvent struct {
	PRKey   string                `json:"prKey"`
	Detail  gitops.PRDetail       `json:"detail"`
	Threads []gitops.ReviewThread `json:"threads"`
	HeadSHA string                `json:"headSHA"`
	Error   string                `json:"error,omitempty"`
	Seq     uint64                `json:"seq"`
}

type prUpdateSnapshot struct {
	Detail  gitops.PRDetail       `json:"detail"`
	Threads []gitops.ReviewThread `json:"threads"`
}

// prUpdatePump is the App's per-PR poller: one ticker, one
// change-detection state, one wire stream, shared by every caller of that
// PR via refs. The goroutine exits when done is closed (the last caller
// unsubscribed) or the app's lifetime context ends.
//
// refs, active, dead, last, lastSnapshot, lastErr, lastWireErr and seq are
// all guarded by App.prUpdates.mu. The change-detection state is under
// the lock rather than owned by the goroutine because a JOINING subscriber
// reads it: it is handed the pump's own snapshot, its own active failure,
// and the sequence both were stamped with, instead of fetching one — which
// is what makes "one PR, one snapshot" true across subscribers rather than
// merely true per fetch. The poll's critical section is a compare and a few
// assignments — the fetch itself stays outside it. `dead` is set by the
// goroutine's own teardown: a pump whose loop exited (app lifetime context,
// panic) polls nothing ever again, so a caller must not be handed a
// reference on it — it would get a subscription that never emits and a
// visibility vote nothing reads.
type prUpdatePump struct {
	prKey string
	pr    gitops.PRReference
	done  chan struct{}
	dead  bool
	// paused suspends polling while EVERY subscriber of this PR reports
	// itself hidden (window minimized, tab in the background) — active is
	// the count of those that don't, so one visible client keeps the pump
	// running for all of them. The pump and its change-detection state
	// stay alive; wake nudges the loop on resume so a poll missed while
	// paused runs immediately instead of waiting out the next tick.
	paused atomic.Bool
	wake   chan struct{}
	refs   int
	active int

	// last is the encoded form used for change detection; lastSnapshot is
	// the same observation decoded, kept so a joiner is served without
	// re-parsing (and without re-fetching). lastErr is the RAW fetch error
	// and exists only as the dedup key — it never reaches the wire;
	// lastWireErr is the redacted summary the matching frame carried, which
	// is what a joiner is handed. seq stamps the state this pump currently
	// holds, so a frame is orderable against the reference a joiner took.
	last         []byte
	lastSnapshot prUpdateSnapshot
	lastErr      string
	lastWireErr  string
	seq          uint64
}

// prUpdateReference is one caller's take on a pump: the handle plus the
// pump state AS OF the moment the reference was registered, all read in the
// one critical section so the three cannot describe different observations.
type prUpdateReference struct {
	id       string
	snapshot prUpdateSnapshot
	wireErr  string
	seq      uint64
}

// prUpdateHandle is one caller's reference on a pump. active mirrors the
// caller's last SetPRUpdatesActive report so a repeated call cannot
// double-count, and an unsubscribe releases exactly what that caller held.
//
// It names the pump by POINTER, not by PR key: a dead pump is replaced in
// the map by a fresh one under the same key, and a key-addressed handle
// would then decrement — and eventually tear down — a pump it never
// referenced.
type prUpdateHandle struct {
	pump   *prUpdatePump
	active bool
}

// prUpdateKey is the entity key for a pull/merge request: forge, project
// path, number. It is the frontend's PR sourceKey minus its "pr:" prefix,
// deliberately — one spelling of "which PR" across the wire.
func prUpdateKey(pr gitops.PRReference) string {
	return fmt.Sprintf("%s:%s:%d", pr.Forge, pr.Project(), pr.Number)
}

//ao:scope git:operate
//ao:route selected
func (a *App) GetPRDetail(pr gitops.PRReference) (gitops.PRDetail, error) {
	if a.shuttingDown.Load() {
		return gitops.PRDetail{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return gitops.PRDetail{}, err
	}
	return a.gitCore().GetPRDetail("", pr)
}

//ao:scope git:operate
func (a *App) GetPRDiff(ws WorkspaceRef, pr gitops.PRReference, baseRef string) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return "", err
	}
	// Prefer a locally-computed diff: gh/glab's PR-diff endpoints refuse
	// diffs over 20k lines (HTTP 406), which large PRs blow past. When the
	// caller has a clone and we know the base ref, we can fetch the PR head
	// + base and diff them from local objects with no such cap. The forge
	// API stays the fallback for a zero ref — a pr-anchor thread with no
	// local checkout.
	if diff, attempted, err := a.localPRDiff(ws, pr, baseRef); attempted {
		return diff, err
	}
	return a.gitCore().GetPRDiff("", pr)
}

// localPRDiff computes the PR diff from local git objects. attempted=false
// means the local path was not viable (no clone or no base ref) and the
// caller should fall back to the forge API; attempted=true returns the
// local result (or its error) authoritatively.
func (a *App) localPRDiff(ws WorkspaceRef, pr gitops.PRReference, baseRef string) (diff string, attempted bool, err error) {
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		return "", false, nil
	}
	workspace, ok := a.localCloneWorkspace(ws)
	if !ok {
		return "", false, nil
	}
	if err := gitops.ValidateBranchName(baseRef); err != nil {
		return "", true, err
	}
	headOID, err := a.fetchPRHeadAndBase(workspace, pr, baseRef)
	if err != nil {
		return "", true, err
	}
	diff, err = a.gitCore().DiffMergeBase(workspace, "origin/"+baseRef, headOID)
	if err != nil {
		return "", true, err
	}
	return diff, true, nil
}

// ListPRCommits returns the commits a PR carries (`origin/base..head`,
// newest first), computed from the referenced local clone. Empty — not
// an error — for a zero ref (a pr-anchor thread with no checkout): the
// frontend hides the commit selector instead of failing the PR load.
//
// headSHA is an optimization contract, not a filter: when the caller
// already knows the PR head OID (GetPRDiff fetched it moments earlier)
// and that commit plus the base branch are present locally, the fetch
// round-trips are skipped. Empty, unknown, or not-yet-fetched values
// fall back to a full fetch.
//
//ao:scope git:operate
func (a *App) ListPRCommits(ws WorkspaceRef, pr gitops.PRReference, baseRef, headSHA string) ([]BranchCommit, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return nil, err
	}
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		return nil, errors.New("base branch is required")
	}
	workspace, ok := a.localCloneWorkspace(ws)
	if !ok {
		return []BranchCommit{}, nil
	}
	if err := gitops.ValidateBranchName(baseRef); err != nil {
		return nil, err
	}
	headOID := strings.TrimSpace(headSHA)
	if !gitdiff.RevisionsExist(context.Background(), workspace, headOID, "refs/remotes/origin/"+baseRef) {
		var err error
		headOID, err = a.fetchPRHeadAndBase(workspace, pr, baseRef)
		if err != nil {
			return nil, err
		}
	}
	commits, err := gitdiff.ListCommitsRange(context.Background(), workspace, "origin/"+baseRef, headOID)
	if err != nil {
		return nil, err
	}
	return commits, nil
}

// GetPRCommitDiff returns the unified patch a single PR commit
// introduced (first-parent diff), read from the referenced local clone.
// Requires a clone — the selector that feeds it only renders when
// ListPRCommits found one.
//
//ao:scope git:operate
func (a *App) GetPRCommitDiff(ws WorkspaceRef, pr gitops.PRReference, sha string, ignoreWhitespace bool) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return "", err
	}
	workspace, ok := a.localCloneWorkspace(ws)
	if !ok {
		return "", errors.New("viewing a PR commit requires a local clone")
	}
	// The commit almost always sits in the clone already (ListPRCommits
	// just fetched the head), so only fetch the PR head — the case where
	// this call races a push or lands in a fresh session — when the
	// commit is missing locally.
	if !gitdiff.RevisionsExist(context.Background(), workspace, sha) {
		headRef, err := gitops.PRHeadRef(pr.Forge, pr.Number)
		if err != nil {
			return "", err
		}
		if _, err := a.gitCore().FetchRefOID(workspace, "origin", headRef); err != nil {
			return "", fmt.Errorf("fetch PR head: %w", err)
		}
	}
	patch, err := gitdiff.CommitDiff(context.Background(), workspace, sha,
		gitdiff.Options{IgnoreWhitespace: ignoreWhitespace})
	if err != nil {
		return "", err
	}
	return string(patch), nil
}

// fetchPRHeadAndBase fetches the PR head ref and base branch into the
// local clone's origin remote and returns the head OID.
func (a *App) fetchPRHeadAndBase(workspace string, pr gitops.PRReference, baseRef string) (string, error) {
	headRef, err := gitops.PRHeadRef(pr.Forge, pr.Number)
	if err != nil {
		return "", err
	}
	core := a.gitCore()
	headOID, err := core.FetchRefOID(workspace, "origin", headRef)
	if err != nil {
		return "", fmt.Errorf("fetch PR head: %w", err)
	}
	if err := core.FetchBranch(workspace, "origin", baseRef); err != nil {
		return "", fmt.Errorf("fetch base branch: %w", err)
	}
	return headOID, nil
}

//ao:scope git:operate
//ao:route selected
func (a *App) ListPRReviewThreads(pr gitops.PRReference) ([]gitops.ReviewThread, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return nil, err
	}
	return a.gitCore().ListReviewThreads("", pr)
}

type SubmitPRReviewResult struct {
	PostedReview       bool   `json:"postedReview"`
	PostedFileComments int    `json:"postedFileComments"`
	PartialFailurePath string `json:"partialFailurePath,omitempty"`
	PartialFailure     string `json:"partialFailure,omitempty"`
}

func mapSubmitPRReviewResult(result gitops.SubmitReviewResult, err error) (SubmitPRReviewResult, error) {
	if err == nil {
		return SubmitPRReviewResult{
			PostedReview:       result.PostedReview,
			PostedFileComments: result.PostedFileComments,
		}, nil
	}
	var partial *gitops.PartialSubmitError
	if errors.As(err, &partial) {
		return SubmitPRReviewResult{
			PostedReview:       partial.PostedReview,
			PostedFileComments: partial.PostedFileComments,
			PartialFailurePath: partial.FailedPath,
			PartialFailure:     partial.Err.Error(),
		}, nil
	}
	return SubmitPRReviewResult{}, err
}

//ao:scope git:operate
//ao:route selected
func (a *App) SubmitPRReview(pr gitops.PRReference, review gitops.SubmitReviewRequest) (SubmitPRReviewResult, error) {
	if a.shuttingDown.Load() {
		return SubmitPRReviewResult{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return SubmitPRReviewResult{}, err
	}
	result, err := a.gitCore().SubmitReview("", pr, review)
	return mapSubmitPRReviewResult(result, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) ReplyToPRThread(pr gitops.PRReference, threadID string, databaseID int64, body string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return err
	}
	return a.gitCore().ReplyToThread("", pr, threadID, databaseID, body)
}

// SetPRThreadResolved resolves (or reopens) one review thread on the
// forge. threadID is the id ListPRReviewThreads reported for it — a
// GitHub review thread node id, a GitLab discussion id.
//
// The answer is the forge's, not a local flag: the next poll of
// SubscribePRUpdates re-reads the thread and the pane follows it, so a
// failure here leaves the badge showing what the forge actually holds.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRThreadResolved(pr gitops.PRReference, threadID string, resolved bool) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return err
	}
	if strings.TrimSpace(threadID) == "" {
		return errors.New("review thread id is required")
	}
	return a.gitCore().SetThreadResolved("", pr, threadID, resolved)
}

// SubscribePRUpdates begins polling a pull request for detail/review-thread
// changes and returns the current snapshot plus the handle that releases it.
//
// One PR gets one pump however many callers subscribe to it: the first
// acquire starts the ticker, later ones take a reference, and the last
// release tears it down. The subscription is also released automatically
// when the calling WS connection drops (transport.ConnState cleanup); the
// frontend SHOULD still unsubscribe on unmount — the connection-tied
// cleanup is the safety net for unclean disconnects.
//
// A JOINER DOES NOT FETCH. The pump already holds the PR's current
// snapshot, so a second pane opening the same PR costs zero gh/glab
// processes and — more importantly — cannot be handed a DIFFERENT snapshot
// than the one every other subscriber of that pump is showing. One pump,
// one snapshot: the fetch happens only on the path that creates a pump, and
// even there a pump that appeared concurrently wins and its snapshot is
// what the caller gets.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SubscribePRUpdates(ctx context.Context, pr gitops.PRReference) (PRUpdateSubscriptionResult, error) {
	if a.shuttingDown.Load() {
		return PRUpdateSubscriptionResult{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return PRUpdateSubscriptionResult{}, err
	}
	prKey := prUpdateKey(pr)

	// Join first, and REFUSE first: both are decided before any forge call,
	// so neither an ordinary second pane nor a caller past the handle cap
	// spawns a process.
	ref, joined, err := a.joinPRUpdatePump(prKey)
	if err != nil {
		return PRUpdateSubscriptionResult{}, err
	}
	var start *prUpdatePump
	if !joined {
		// Fetched before the pump is registered so a failing forge call
		// fails the subscribe outright instead of leaving a pump nobody
		// can see the first result of.
		fetched, err := a.fetchPRUpdateSnapshot(pr)
		if err != nil {
			return PRUpdateSubscriptionResult{}, err
		}
		encoded, err := encodePRUpdateSnapshot(fetched)
		if err != nil {
			return PRUpdateSubscriptionResult{}, err
		}
		ref, start, err = a.createPRUpdatePump(pr, prKey, fetched, encoded)
		if err != nil {
			return PRUpdateSubscriptionResult{}, err
		}
	}
	if start != nil {
		a.prUpdates.wg.Go(func() { a.pumpPRUpdates(start) })
	}

	if state := transport.ConnStateFromContext(ctx); state != nil {
		// A false return means the connection is already tearing down —
		// the safety net it would have provided is gone, so release the
		// reference now rather than leak a pump until shutdown.
		if !state.RegisterCleanup(func() { a.unsubscribePRUpdates(ref.id) }) {
			a.unsubscribePRUpdates(ref.id)
			return PRUpdateSubscriptionResult{}, fmt.Errorf("pr updates: connection closing")
		}
	}

	return PRUpdateSubscriptionResult{
		ID:      ref.id,
		PRKey:   prKey,
		Detail:  ref.snapshot.Detail,
		Threads: ref.snapshot.Threads,
		HeadSHA: ref.snapshot.Detail.HeadSHA,
		Error:   ref.wireErr,
		Seq:     ref.seq,
	}, nil
}

// joinPRUpdatePump takes a reference on the live pump for prKey, if there
// is one, and hands back ITS snapshot. joined=false means the caller must
// fetch and create one. The handle cap is enforced here so a refusal costs
// nothing.
func (a *App) joinPRUpdatePump(prKey string) (ref prUpdateReference, joined bool, err error) {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	a.ensurePRUpdateMapsLocked()
	if len(a.prUpdates.handles) >= maxPRUpdateHandles {
		return prUpdateReference{}, false, ErrTooManyPRUpdateSubscriptions
	}
	pump := a.livePRUpdatePumpLocked(prKey)
	if pump == nil {
		return prUpdateReference{}, false, nil
	}
	return a.takePRUpdateReferenceLocked(pump), true, nil
}

// createPRUpdatePump registers a pump seeded with the caller's fetch. The
// cap and the pump map are re-checked because the fetch happened without
// the lock: a pump created concurrently WINS, and the caller joins it and
// takes its snapshot rather than being handed the one it just fetched —
// two subscribers of one pump must never start from different snapshots,
// however narrowly they raced. start is non-nil only when a pump was
// actually created and its goroutine still has to be launched.
func (a *App) createPRUpdatePump(
	pr gitops.PRReference,
	prKey string,
	snapshot prUpdateSnapshot,
	encoded []byte,
) (ref prUpdateReference, start *prUpdatePump, err error) {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	a.ensurePRUpdateMapsLocked()
	if len(a.prUpdates.handles) >= maxPRUpdateHandles {
		return prUpdateReference{}, nil, ErrTooManyPRUpdateSubscriptions
	}
	if existing := a.livePRUpdatePumpLocked(prKey); existing != nil {
		return a.takePRUpdateReferenceLocked(existing), nil, nil
	}
	pump := &prUpdatePump{
		prKey:        prKey,
		pr:           pr,
		done:         make(chan struct{}),
		wake:         make(chan struct{}, 1),
		last:         encoded,
		lastSnapshot: snapshot,
		seq:          a.nextPRUpdateSeqLocked(),
	}
	a.prUpdates.pumps[prKey] = pump
	return a.takePRUpdateReferenceLocked(pump), pump, nil
}

// nextPRUpdateSeqLocked stamps the next pump state. Callers hold
// a.prUpdates.mu, which is also what orders the stamp against the store it
// belongs to: a frame's seq is assigned in the same critical section that
// published the state it carries.
func (a *App) nextPRUpdateSeqLocked() uint64 {
	a.prUpdates.seq++
	return a.prUpdates.seq
}

func (a *App) ensurePRUpdateMapsLocked() {
	if a.prUpdates.pumps == nil {
		a.prUpdates.pumps = make(map[string]*prUpdatePump)
	}
	if a.prUpdates.handles == nil {
		a.prUpdates.handles = make(map[string]*prUpdateHandle)
	}
}

// livePRUpdatePumpLocked returns the pump serving prKey, or nil when there
// is none or the one there is dead. A dead pump reads as absent: its
// goroutine has already stopped polling, so sharing it would hand back a
// handle that receives nothing. The fresh pump replaces the map entry and
// the dead one's own drop leaves it alone (it checks identity) while still
// releasing exactly the handles that referenced IT.
func (a *App) livePRUpdatePumpLocked(prKey string) *prUpdatePump {
	pump, ok := a.prUpdates.pumps[prKey]
	if !ok || pump.dead {
		return nil
	}
	return pump
}

// takePRUpdateReferenceLocked registers one handle against pump and returns
// it with the pump's current snapshot. A new subscriber is visible until it
// says otherwise, so a pump paused by the last hidden client resumes for it
// — and resuming nudges the loop, or the poll this subscriber is waiting
// for waits out a whole tick that the pause already made it miss.
func (a *App) takePRUpdateReferenceLocked(pump *prUpdatePump) prUpdateReference {
	id := uuid.NewString()
	pump.refs++
	pump.active++
	resumed := pump.refs > 1 && pump.active == 1
	pump.paused.Store(false)
	a.prUpdates.handles[id] = &prUpdateHandle{pump: pump, active: true}
	if resumed {
		wakePRUpdatePump(pump)
	}
	return prUpdateReference{
		id:       id,
		snapshot: pump.lastSnapshot,
		wireErr:  pump.lastWireErr,
		seq:      pump.seq,
	}
}

//ao:scope git:operate
//ao:route home
func (a *App) UnsubscribePRUpdates(subscriptionID string) error {
	a.unsubscribePRUpdates(subscriptionID)
	return nil
}

// SetPRUpdatesActive reports whether ONE subscriber currently wants its PR
// polled. The frontend drives it from document visibility so a hidden
// window stops spawning gh/glab every tick.
//
// The reports compose: a PR's pump polls while ANY of its subscribers is
// active, and pauses only once every one of them has gone quiet. Each
// handle remembers its own last report, so repeated calls in the same
// direction are idempotent and a hidden client that unsubscribes cannot
// leave a phantom vote behind.
//
// An unknown id is a no-op, not an error: visibility flips race scope
// switches and pane disposal, and losing that race is benign.
//
//ao:scope git:operate
//ao:route home
func (a *App) SetPRUpdatesActive(subscriptionID string, active bool) error {
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.handles[subscriptionID]
	if !ok || handle.active == active {
		a.prUpdates.mu.Unlock()
		return nil
	}
	handle.active = active
	pump := handle.pump
	if pump.dead {
		a.prUpdates.mu.Unlock()
		return nil
	}
	if active {
		pump.active++
	} else {
		pump.active--
	}
	resumed := active && pump.active == 1
	pump.paused.Store(pump.active <= 0)
	a.prUpdates.mu.Unlock()
	if resumed {
		wakePRUpdatePump(pump)
	}
	return nil
}

// wakePRUpdatePump nudges a resumed pump's loop without blocking: the
// channel is capacity-1, and a wake already queued does the same job.
func wakePRUpdatePump(pump *prUpdatePump) {
	select {
	case pump.wake <- struct{}{}:
	default:
	}
}

func (a *App) pumpPRUpdates(pump *prUpdatePump) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("pr updates: pump panic for pr=%s: %v", pump.prKey, r)
		}
		// Self-clean if we exited on the app lifetime context or a panic.
		// The Unsubscribe path already dropped the pump before reaching
		// here; dropping again is a benign no-op.
		a.dropPRUpdatePump(pump)
	}()
	interval := a.prUpdatePollInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// missedTick makes resume-after-pause cheap for quick hide/show flips:
	// a wake only triggers an immediate poll when a tick actually elapsed
	// while paused; otherwise the regular ticker cadence resumes untouched.
	missedTick := false
	for {
		select {
		case <-pump.done:
			return
		case <-a.lifeCtx().Done():
			return
		case <-pump.wake:
			if pump.paused.Load() || !missedTick {
				continue
			}
			missedTick = false
		case <-ticker.C:
			if pump.paused.Load() {
				missedTick = true
				continue
			}
			missedTick = false
		}
		event, changed := a.pollPRUpdate(pump)
		if !changed {
			continue
		}
		select {
		case <-pump.done:
			return
		default:
		}
		a.emit(eventchan.PRUpdated, event)
	}
}

// pollPRUpdate fetches one snapshot and folds it into the pump's change
// detection. changed=false means the observation is identical to the last
// one broadcast — including a repeat of the same failure, which must not
// re-emit every tick while a forge is down.
//
// A dead pump stores nothing, stamps nothing and emits nothing, in either
// critical section. Per PR key, sequence order must equal content order —
// that is the whole basis of the frontend's watermark — and a detached
// pump's in-flight poll began BEFORE its replacement's, so a stamp taken
// after the replacement was created would outrank fresher content. With the
// guard, a pump only stores while live and successive pumps' live windows
// for a key cannot overlap.
func (a *App) pollPRUpdate(pump *prUpdatePump) (PRUpdatedEvent, bool) {
	snapshot, err := a.fetchPRUpdateSnapshot(pump.pr)
	if err == nil {
		var encoded []byte
		encoded, err = encodePRUpdateSnapshot(snapshot)
		if err == nil {
			// The fetch ran outside the lock; only the compare-and-store is
			// inside it, because a joining subscriber reads this state.
			a.prUpdates.mu.Lock()
			if pump.dead {
				a.prUpdates.mu.Unlock()
				return PRUpdatedEvent{}, false
			}
			unchanged := string(encoded) == string(pump.last) && pump.lastErr == ""
			var seq uint64
			if !unchanged {
				pump.last = encoded
				pump.lastSnapshot = snapshot
				pump.lastErr = ""
				pump.lastWireErr = ""
				pump.seq = a.nextPRUpdateSeqLocked()
				seq = pump.seq
			}
			a.prUpdates.mu.Unlock()
			if unchanged {
				return PRUpdatedEvent{}, false
			}
			return PRUpdatedEvent{
				PRKey:   pump.prKey,
				Detail:  snapshot.Detail,
				Threads: snapshot.Threads,
				HeadSHA: snapshot.Detail.HeadSHA,
				Seq:     seq,
			}, true
		}
	}
	// The verbatim text is gh/glab's own stderr — remote URLs, tokens
	// echoed back by a failed auth call, local clone paths — and
	// "pr:updated" reaches every subscriber of the PR. The wire gets a
	// caller-safe summary plus a correlation id; the full text stays in
	// the server log, the same split internal/transport makes for RPC
	// errors. The id is minted before the lock so the summary STORED for
	// joiners and the one EMITTED here are the same string.
	message := err.Error()
	correlationID := uuid.NewString()
	wireErr := prUpdateErrorMessage(correlationID)
	// Dedup BEFORE logging: a forge that is down fails identically every
	// tick, and logging first turned one outage into a log line every 45s
	// for as long as a pane stayed open.
	a.prUpdates.mu.Lock()
	if pump.dead {
		a.prUpdates.mu.Unlock()
		return PRUpdatedEvent{}, false
	}
	duplicate := pump.lastErr == message
	var seq uint64
	if !duplicate {
		pump.lastErr = message
		pump.lastWireErr = wireErr
		pump.seq = a.nextPRUpdateSeqLocked()
		seq = pump.seq
	}
	a.prUpdates.mu.Unlock()
	if duplicate {
		return PRUpdatedEvent{}, false
	}
	log.Printf("pr updates: poll failed for pr=%s (id: %s): %v", pump.prKey, correlationID, err)
	// last is deliberately left alone: the failure does not invalidate the
	// snapshot consumers are still showing, and a recovery that returns
	// the same content must re-emit to clear the error, which the
	// lastErr check above handles.
	return PRUpdatedEvent{PRKey: pump.prKey, Error: wireErr, Seq: seq}, true
}

// prUpdateErrorMessage is the only PR-poll failure text that reaches the
// wire: what went wrong, and the id to grep the server log for.
func prUpdateErrorMessage(correlationID string) string {
	return fmt.Sprintf("failed to refresh pull request (id: %s)", correlationID)
}

// unsubscribePRUpdates releases one caller's handle. The pump (and its
// change-detection state) survives until the last handle goes. Idempotent —
// unknown ids and double-unsubscribes are no-ops, because the
// connection-cleanup safety net may have run first on disconnect.
func (a *App) unsubscribePRUpdates(id string) {
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.handles[id]
	if !ok {
		a.prUpdates.mu.Unlock()
		return
	}
	delete(a.prUpdates.handles, id)
	var teardown *prUpdatePump
	pump := handle.pump
	if handle.active {
		pump.active--
	}
	pump.refs--
	if pump.refs <= 0 {
		teardown = pump
		// Stamped under the same lock pollPRUpdate stores under, so a poll
		// this pump still has in flight cannot stamp state (or a sequence)
		// after a replacement pump exists — per key, sequence order must
		// match content order, and this pump's fetch began before any
		// successor's. dropPRUpdatePump re-stamps harmlessly.
		pump.dead = true
		// Only if it is still the pump serving this key — a superseded
		// (dead) pump was replaced in the map and must not evict its
		// successor.
		if a.prUpdates.pumps[pump.prKey] == pump {
			delete(a.prUpdates.pumps, pump.prKey)
		}
	} else {
		pump.paused.Store(pump.active <= 0)
	}
	a.prUpdates.mu.Unlock()
	if teardown != nil {
		close(teardown.done)
	}
}

// dropPRUpdatePump removes a pump whose goroutine exited on its own (app
// lifetime context, panic) along with every handle that pointed at it —
// those handles name a stream that no longer exists.
//
// The `dead` stamp goes on FIRST and unconditionally, so a Subscribe that
// takes the lock after this point mints a fresh pump instead of taking a
// reference on a goroutine that has stopped. The residual window is a
// Subscribe that took the lock BEFORE this ran: it is unclosable from here
// (the goroutine cannot mark the pump under the lock any earlier than its
// own teardown), and it is shutdown-only — the loop exits on the app
// lifetime context, after which nothing new subscribes.
func (a *App) dropPRUpdatePump(pump *prUpdatePump) {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	pump.dead = true
	if a.prUpdates.pumps[pump.prKey] == pump {
		delete(a.prUpdates.pumps, pump.prKey)
	}
	for id, handle := range a.prUpdates.handles {
		if handle.pump == pump {
			delete(a.prUpdates.handles, id)
		}
	}
}

func (a *App) closePRUpdatePumps() {
	a.prUpdates.mu.Lock()
	ids := make([]string, 0, len(a.prUpdates.handles))
	for id := range a.prUpdates.handles {
		ids = append(ids, id)
	}
	a.prUpdates.mu.Unlock()
	for _, id := range ids {
		a.unsubscribePRUpdates(id)
	}
	// A pump whose handles all vanished with a dropped connection can
	// outlive them; close what is left so Wait() cannot block.
	a.prUpdates.mu.Lock()
	orphans := make([]*prUpdatePump, 0, len(a.prUpdates.pumps))
	for key, pump := range a.prUpdates.pumps {
		delete(a.prUpdates.pumps, key)
		orphans = append(orphans, pump)
	}
	a.prUpdates.mu.Unlock()
	for _, pump := range orphans {
		close(pump.done)
	}
	a.prUpdates.wg.Wait()
}

func (a *App) fetchPRUpdateSnapshot(pr gitops.PRReference) (prUpdateSnapshot, error) {
	if a.prUpdates.fetchFn != nil {
		return a.prUpdates.fetchFn(pr)
	}
	detail, err := a.gitCore().GetPRDetail("", pr)
	if err != nil {
		return prUpdateSnapshot{}, err
	}
	threads, err := a.gitCore().ListReviewThreads("", pr)
	if err != nil {
		return prUpdateSnapshot{}, err
	}
	return prUpdateSnapshot{Detail: detail, Threads: threads}, nil
}

func (a *App) prUpdatePollInterval() time.Duration {
	if a.prUpdates.interval > 0 {
		return a.prUpdates.interval
	}
	return defaultPRUpdateInterval
}

func encodePRUpdateSnapshot(snapshot prUpdateSnapshot) ([]byte, error) {
	return json.Marshal(snapshot)
}

func validatePRReference(pr gitops.PRReference) error {
	if pr.Number <= 0 {
		return fmt.Errorf("PR number must be positive, got %d", pr.Number)
	}
	_, _, err := gitops.SplitProjectForForge(pr.Forge, pr.Project())
	return err
}

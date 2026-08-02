package main

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

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

const defaultPRUpdateInterval = 45 * time.Second

type PRUpdateSubscriptionResult struct {
	ID       string                `json:"id"`
	ThreadID string                `json:"threadId"`
	PR       gitops.PRReference    `json:"pr"`
	Detail   gitops.PRDetail       `json:"detail"`
	Threads  []gitops.ReviewThread `json:"threads"`
	HeadSHA  string                `json:"headSHA"`
}

type PRUpdatedEvent struct {
	SubscriptionID string                `json:"subscriptionId"`
	ThreadID       string                `json:"threadId"`
	PR             gitops.PRReference    `json:"pr"`
	Detail         gitops.PRDetail       `json:"detail"`
	Threads        []gitops.ReviewThread `json:"threads"`
	HeadSHA        string                `json:"headSHA"`
}

type prUpdateSnapshot struct {
	Detail  gitops.PRDetail       `json:"detail"`
	Threads []gitops.ReviewThread `json:"threads"`
}

type prUpdatePump struct {
	threadID string
	pr       gitops.PRReference
	done     chan struct{}
	// paused suspends polling while every client looking at this
	// subscription reports itself hidden (window minimized, tab in the
	// background). The pump and its last-snapshot diff state stay alive;
	// wake nudges the loop on resume so a poll missed while paused runs
	// immediately instead of waiting out the next tick.
	paused atomic.Bool
	wake   chan struct{}
	last   []byte
}

func (a *App) GetPRDetail(pr gitops.PRReference) (gitops.PRDetail, error) {
	if a.shuttingDown.Load() {
		return gitops.PRDetail{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return gitops.PRDetail{}, err
	}
	return a.gitCore().GetPRDetail("", pr)
}

func (a *App) GetPRDiff(threadID string, pr gitops.PRReference, baseRef string) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return "", err
	}
	// Prefer a locally-computed diff: gh/glab's PR-diff endpoints refuse
	// diffs over 20k lines (HTTP 406), which large PRs blow past. When the
	// thread has a clone and we know the base ref, we can fetch the PR head
	// + base and diff them from local objects with no such cap. The forge
	// API stays the fallback for pr-anchor threads with no local checkout.
	if diff, attempted, err := a.localPRDiff(threadID, pr, baseRef); attempted {
		return diff, err
	}
	return a.gitCore().GetPRDiff("", pr)
}

// localPRDiff computes the PR diff from local git objects. attempted=false
// means the local path was not viable (no clone or no base ref) and the
// caller should fall back to the forge API; attempted=true returns the
// local result (or its error) authoritatively.
func (a *App) localPRDiff(threadID string, pr gitops.PRReference, baseRef string) (diff string, attempted bool, err error) {
	baseRef = strings.TrimSpace(baseRef)
	if threadID == "" || baseRef == "" {
		return "", false, nil
	}
	workspace, ok := a.localCloneWorkspace(threadID)
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
// newest first), computed from the thread's local clone. Empty — not
// an error — when the thread has no local clone (a pr-anchor thread
// with no checkout): the frontend hides the commit selector instead of
// failing the PR load.
//
// headSHA is an optimization contract, not a filter: when the caller
// already knows the PR head OID (GetPRDiff fetched it moments earlier)
// and that commit plus the base branch are present locally, the fetch
// round-trips are skipped. Empty, unknown, or not-yet-fetched values
// fall back to a full fetch.
func (a *App) ListPRCommits(threadID string, pr gitops.PRReference, baseRef, headSHA string) ([]BranchCommit, error) {
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
	workspace, ok := a.localCloneWorkspace(threadID)
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
// introduced (first-parent diff), read from the thread's local clone.
// Requires a clone — the selector that feeds it only renders when
// ListPRCommits found one.
func (a *App) GetPRCommitDiff(threadID string, pr gitops.PRReference, sha string, ignoreWhitespace bool) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return "", err
	}
	workspace, ok := a.localCloneWorkspace(threadID)
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

func (a *App) ReplyToPRThread(pr gitops.PRReference, threadID string, databaseID int64, body string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return err
	}
	return a.gitCore().ReplyToThread("", pr, threadID, databaseID, body)
}

func (a *App) SubscribePRUpdates(ctx context.Context, threadID string, pr gitops.PRReference) (PRUpdateSubscriptionResult, error) {
	if a.shuttingDown.Load() {
		return PRUpdateSubscriptionResult{}, ErrShuttingDown
	}
	if threadID == "" {
		return PRUpdateSubscriptionResult{}, errors.New("threadID is required")
	}
	if err := validatePRReference(pr); err != nil {
		return PRUpdateSubscriptionResult{}, err
	}
	snapshot, err := a.fetchPRUpdateSnapshot(pr)
	if err != nil {
		return PRUpdateSubscriptionResult{}, err
	}
	encoded, err := encodePRUpdateSnapshot(snapshot)
	if err != nil {
		return PRUpdateSubscriptionResult{}, err
	}
	id := uuid.NewString()
	entry := &prUpdatePump{
		threadID: threadID,
		pr:       pr,
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		last:     encoded,
	}
	a.prUpdatePumpsMu.Lock()
	if a.prUpdatePumps == nil {
		a.prUpdatePumps = make(map[string]*prUpdatePump)
	}
	a.prUpdatePumps[id] = entry
	a.prUpdatePumpsMu.Unlock()
	a.prUpdatePumpWG.Go(func() { a.pumpPRUpdates(id, entry) })

	if state := transport.ConnStateFromContext(ctx); state != nil {
		if !state.RegisterCleanup(func() { a.unsubscribePRUpdates(id) }) {
			a.unsubscribePRUpdates(id)
			return PRUpdateSubscriptionResult{}, fmt.Errorf("pr updates: connection closing")
		}
	}

	return PRUpdateSubscriptionResult{
		ID:       id,
		ThreadID: threadID,
		PR:       pr,
		Detail:   snapshot.Detail,
		Threads:  snapshot.Threads,
		HeadSHA:  snapshot.Detail.HeadSHA,
	}, nil
}

func (a *App) UnsubscribePRUpdates(subscriptionID string) error {
	a.unsubscribePRUpdates(subscriptionID)
	return nil
}

// SetPRUpdatesActive pauses (active=false) or resumes (active=true) a
// subscription's poll pump. The frontend drives it from document
// visibility so a hidden window stops spawning gh/glab every tick. An
// unknown id is a no-op, not an error: visibility flips race scope
// switches and pane disposal, and losing that race is benign.
func (a *App) SetPRUpdatesActive(subscriptionID string, active bool) error {
	a.prUpdatePumpsMu.Lock()
	entry, ok := a.prUpdatePumps[subscriptionID]
	a.prUpdatePumpsMu.Unlock()
	if !ok {
		return nil
	}
	entry.paused.Store(!active)
	if active {
		select {
		case entry.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (a *App) pumpPRUpdates(id string, entry *prUpdatePump) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("pr updates: pump panic for id=%s: %v", id, r)
		}
		a.prUpdatePumpsMu.Lock()
		delete(a.prUpdatePumps, id)
		a.prUpdatePumpsMu.Unlock()
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
		case <-entry.done:
			return
		case <-a.lifeCtx().Done():
			return
		case <-entry.wake:
			if entry.paused.Load() || !missedTick {
				continue
			}
			missedTick = false
		case <-ticker.C:
			if entry.paused.Load() {
				missedTick = true
				continue
			}
			missedTick = false
		}
		snapshot, err := a.fetchPRUpdateSnapshot(entry.pr)
		if err != nil {
			log.Printf("pr updates: fetch failed for id=%s forge=%s project=%s number=%d: %v", id, entry.pr.Forge, entry.pr.Project(), entry.pr.Number, err)
			continue
		}
		encoded, err := encodePRUpdateSnapshot(snapshot)
		if err != nil {
			log.Printf("pr updates: encode failed for id=%s: %v", id, err)
			continue
		}
		if string(encoded) == string(entry.last) {
			continue
		}
		entry.last = encoded
		select {
		case <-entry.done:
			return
		default:
		}
		a.emit("pr:updated", PRUpdatedEvent{
			SubscriptionID: id,
			ThreadID:       entry.threadID,
			PR:             entry.pr,
			Detail:         snapshot.Detail,
			Threads:        snapshot.Threads,
			HeadSHA:        snapshot.Detail.HeadSHA,
		})
	}
}

func (a *App) unsubscribePRUpdates(id string) {
	a.prUpdatePumpsMu.Lock()
	entry, ok := a.prUpdatePumps[id]
	if ok {
		delete(a.prUpdatePumps, id)
	}
	a.prUpdatePumpsMu.Unlock()
	if !ok {
		return
	}
	close(entry.done)
}

func (a *App) closePRUpdatePumps() {
	a.prUpdatePumpsMu.Lock()
	ids := make([]string, 0, len(a.prUpdatePumps))
	for id := range a.prUpdatePumps {
		ids = append(ids, id)
	}
	a.prUpdatePumpsMu.Unlock()
	for _, id := range ids {
		a.unsubscribePRUpdates(id)
	}
	a.prUpdatePumpWG.Wait()
}

func (a *App) fetchPRUpdateSnapshot(pr gitops.PRReference) (prUpdateSnapshot, error) {
	if a.prUpdateFetchFn != nil {
		return a.prUpdateFetchFn(pr)
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
	if a.prUpdateInterval > 0 {
		return a.prUpdateInterval
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

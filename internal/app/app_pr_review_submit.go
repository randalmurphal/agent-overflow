package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// --- PR review submit with stored drafts ---
//
// A review whose comments are drafts posts them and marks them sent. Done
// by the pane as two calls, an answer lost between them leaves posted
// drafts unsent, and a retry posts them again. A request that names its
// drafts (SubmitPRReviewRequest.Drafts, capability pr-review-drafts.v1)
// has this backend, which owns the drafts, do both in one call:
//
//   - One submit per PR at a time (reviewSubmitLocks), from reading the
//     drafts through posting and marking. A request without Drafts holds
//     the same lock: on GitLab, whose publish takes every pending draft
//     note of the viewer, an unlocked submit could publish the notes a
//     locked one is still creating, which that one's cleanup would then
//     read as removed. Nothing else creates draft notes (replies and
//     thread actions post notes directly), so nothing else takes it.
//   - A result means this submit reached the forge: Posted says the forge
//     took a review from it, Unconfirmed that the forge's answer did not
//     say (gitops.UnconfirmedSubmitError: its drafts stay drafts and are
//     not held, since nothing proves they posted). An error means it
//     posted nothing. A draft that is not a draft any more (sent by an earlier
//     submit, another tab or device, or a message) is refused with
//     ReviewDraftsNotDraftsCode, and one an earlier review posted that is
//     not marked yet with ReviewDraftsUnmarkedCode, before anything reaches
//     the forge.
//   - The forge comments are built from the stored drafts; the request's
//     own Comments are ignored.
//   - Marking is conditional on what was posted
//     (store.MarkPostedDiffReviewDraftsSent): a draft edited, moved or
//     deleted while the review posted stays as it is, and the result
//     names it (EditedDraftIDs).
//   - A review the forge accepted whose drafts could not be marked keeps
//     them in memory as posted (postedReviewDrafts) and says so in its
//     result (PartialFailureKind "unmarked"). The next submit for the
//     owner retries the marking before it reads anything, and refuses to
//     post while it fails; so does the next list of the owner's drafts.
//   - A request with an AttemptID records its outcome (reviewAttempts):
//     GetPRReviewSubmitOutcome answers it after a lost answer, and a
//     request repeating the id gets the recorded outcome and posts
//     nothing.
//
// What this does not cover: the process ending between the forge's answer
// and the mark leaves posted drafts listed as unsent, with no record that
// they were posted, and forgets every attempt; the pane tells a restarted
// backend by its hello's launchId and says the review may be on the PR.
// The lock is this backend's: two computers holding the same PR's drafts
// do not share it, and on GitLab two computers submitting the same MR at
// once can publish each other's notes.
//
// A request without Drafts is the caller's comments, posted as given; the
// caller marks its drafts with MarkDiffReviewCommentsSent or
// MarkPRDiffReviewCommentsSent.

// Codes of SubmitPRReview's refusals of a draft submit. Both posted nothing.
const (
	// ReviewDraftsNotDraftsCode: a named draft is not a draft any more.
	ReviewDraftsNotDraftsCode = "review_drafts_not_drafts"
	// ReviewDraftsUnmarkedCode: drafts an earlier review posted are not
	// marked sent yet, so this one waits.
	ReviewDraftsUnmarkedCode = "review_drafts_unmarked"
)

// reviewUnmarkedKind is the PartialFailureKind of a review that posted
// with drafts left unmarked.
const reviewUnmarkedKind = "unmarked"

// SubmitPRReviewRequest is one review submit: a verdict, a summary and the
// comments, either built by the caller (Comments) or, with Drafts, read
// from the stored drafts it names.
type SubmitPRReviewRequest struct {
	Verdict  string                     `json:"verdict"`
	Body     string                     `json:"body"`
	Comments []gitops.ReviewLineComment `json:"comments"`
	// Drafts names the stored pr-scope drafts the review carries; Comments
	// is then ignored. Absent from a client older than pr-review-drafts.v1.
	Drafts *SubmitPRReviewDrafts `json:"drafts,omitempty"`
	// AttemptID names this press of Send, fresh for each, so its outcome
	// can be asked for (GetPRReviewSubmitOutcome).
	AttemptID string `json:"attemptId,omitempty"`
}

// SubmitPRReviewDrafts selects the drafts a review posts: a thread's own
// (ThreadID set) or the PR's (ThreadID empty), of the PR's pr-scope source.
type SubmitPRReviewDrafts struct {
	ThreadID string   `json:"threadId,omitempty"`
	IDs      []string `json:"ids"`
	// HeadSHA is the PR head the drafts were written against; a thread's
	// sent drafts record it (`pr:<head>`).
	HeadSHA string `json:"headSha,omitempty"`
}

// SubmitPRReview posts a review of pr. With review.Drafts it posts the
// named drafts and marks the ones the forge received sent (see above).
//
//ao:scope git:operate
//ao:route selected
func (a *App) SubmitPRReview(ctx context.Context, pr gitops.PRReference, review SubmitPRReviewRequest) (result SubmitPRReviewResult, err error) {
	if a.shuttingDown.Load() {
		return SubmitPRReviewResult{}, ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return SubmitPRReviewResult{}, err
	}
	if review.AttemptID != "" {
		attempt, repeated := a.reviewAttempts.begin(review.AttemptID, pr.Identity())
		if repeated && attempt.prKey != pr.Identity() {
			return SubmitPRReviewResult{}, fmt.Errorf("submit PR review: attempt %s was a submit to another PR", review.AttemptID)
		}
		if repeated {
			return attempt.wait(ctx)
		}
		defer func() { a.reviewAttempts.finish(attempt, result, err) }()
	}
	// Every submit to the PR holds its lock, whatever the request's shape:
	// on GitLab one submit's publish takes the draft notes another is
	// still creating.
	unlock, err := a.reviewSubmitLocks().LockCtx(ctx, pr.Identity())
	if err != nil {
		return SubmitPRReviewResult{}, err
	}
	defer unlock()
	if review.Drafts == nil {
		result, err := a.gitCore().SubmitReview(forgeapi.WithInteractive(ctx), pr, gitops.SubmitReviewRequest{Verdict: review.Verdict, Body: review.Body, Comments: review.Comments})
		mapped, err := mapSubmitPRReviewResult(pr, gitops.ActionReview, result, err)
		return mapped, gitops.ExplainForgeError(pr, gitops.ActionReview, err)
	}
	return a.submitPRReviewDrafts(ctx, pr, review)
}

// submitPRReviewDrafts is SubmitPRReview with Drafts, under the PR's lock.
func (a *App) submitPRReviewDrafts(ctx context.Context, pr gitops.PRReference, review SubmitPRReviewRequest) (SubmitPRReviewResult, error) {
	owner := store.PRReviewOwner(pr.Key())
	sentTurnID := ""
	if threadID := review.Drafts.ThreadID; threadID != "" {
		if err := a.requireScope(ctx, transport.ScopeThreadsOperate, "SubmitPRReview marks a thread's drafts sent"); err != nil {
			return SubmitPRReviewResult{}, err
		}
		owner = store.ThreadReviewOwner(threadID)
		sentTurnID = "pr:" + review.Drafts.HeadSHA
	}
	source := prDraftSource(pr)
	if err := a.reconcilePostedReviewDrafts(owner); err != nil {
		return SubmitPRReviewResult{}, err
	}
	drafts, err := a.store.ListDraftDiffReviewCommentsByID(owner, string(store.DiffReviewScopePR), source, review.Drafts.IDs, store.DiffReviewMessageRows{})
	if errors.Is(err, store.ErrDiffReviewCommentsNotDrafts) {
		return SubmitPRReviewResult{}, errorsx.Public(ReviewDraftsNotDraftsCode, "Some of these comments were already sent or removed, so this review was not posted.", err)
	}
	if err != nil {
		return SubmitPRReviewResult{}, fmt.Errorf("submit PR review: %w", err)
	}
	request := gitops.SubmitReviewRequest{Verdict: review.Verdict, Body: review.Body, Comments: make([]gitops.ReviewLineComment, 0, len(drafts))}
	for _, draft := range drafts {
		if comment, ok := reviewLineCommentForDraft(draft); ok {
			request.Comments = append(request.Comments, comment)
		}
	}
	result, err := a.gitCore().SubmitReview(forgeapi.WithInteractive(ctx), pr, request)
	if _, ok := errors.AsType[*gitops.UnconfirmedSubmitError](err); ok {
		slog.Warn("submit PR review: the forge did not confirm the review", "pr", pr.Key(), "err", err)
		return SubmitPRReviewResult{Unconfirmed: true}, nil
	}
	mapped, err := mapSubmitPRReviewResult(pr, gitops.ActionReview, result, err)
	if err != nil {
		return SubmitPRReviewResult{}, gitops.ExplainForgeError(pr, gitops.ActionReview, err)
	}
	// The one place a draft submit says it posted.
	mapped.Posted = mapped.PostedReview || mapped.PostedFileComments > 0
	if !mapped.Posted {
		return mapped, nil
	}
	posted := postedReviewDraftsOf(drafts, mapped)
	sentAt := time.Now().UnixMilli()
	marked, err := a.store.MarkPostedDiffReviewDraftsSent(owner, string(store.DiffReviewScopePR), source, posted, sentAt, sentTurnID)
	if err != nil {
		a.postedReviewDrafts.hold(owner, postedReviewBatch{sourceKey: source, sentTurnID: sentTurnID, sentAt: sentAt, drafts: posted})
		slog.Warn("submit PR review: posted drafts not marked sent", "pr", pr.Key(), "err", err)
		if mapped.PartialFailure == "" {
			mapped.PartialFailure = "Its comments are not marked sent yet. They will not post again, and the app marks them on its next read."
			mapped.PartialFailureKind = reviewUnmarkedKind
		}
		return mapped, nil
	}
	mapped.SentDraftIDs = marked
	for _, draft := range posted {
		if !slices.Contains(marked, draft.ID) {
			mapped.EditedDraftIDs = append(mapped.EditedDraftIDs, draft.ID)
		}
	}
	if owner.ThreadID != "" && len(posted) > 0 {
		a.announceDiffCommentsChanged(owner.ThreadID, string(store.DiffReviewScopePR), source)
	}
	return mapped, nil
}

// PRReviewSubmitOutcome is what a submit attempt did: "posted" (Result),
// "unconfirmed" (Result: the forge did not say), "failed" (Code and
// Message: it posted nothing), "unknown" (this process never received
// it), or "forgotten" (it may have received it and no longer keeps the
// outcome).
type PRReviewSubmitOutcome struct {
	State   string                `json:"state"`
	Result  *SubmitPRReviewResult `json:"result,omitempty"`
	Code    string                `json:"code,omitempty"`
	Message string                `json:"message,omitempty"`
}

// GetPRReviewSubmitOutcome answers what the submit attempt attemptID to pr
// did, waiting for one still running.
//
//ao:scope git:operate
//ao:route selected
//ao:callerbound
func (a *App) GetPRReviewSubmitOutcome(ctx context.Context, pr gitops.PRReference, attemptID string) (PRReviewSubmitOutcome, error) {
	if err := pr.Validate(); err != nil {
		return PRReviewSubmitOutcome{}, err
	}
	attempt, forgotten := a.reviewAttempts.find(attemptID, pr.Identity())
	if forgotten {
		return PRReviewSubmitOutcome{State: "forgotten"}, nil
	}
	if attempt == nil {
		return PRReviewSubmitOutcome{State: "unknown"}, nil
	}
	select {
	case <-attempt.done:
	case <-ctx.Done():
		return PRReviewSubmitOutcome{}, ctx.Err()
	}
	return attempt.outcome, nil
}

// reviewAttemptsKept bounds the finished attempts whose outcome a process
// keeps; reviewAttemptIDsKept the ones after those whose id it keeps.
const (
	reviewAttemptsKept   = 256
	reviewAttemptIDsKept = 4096
)

// reviewAttempts holds the submit attempts this process received: every
// running one and the outcomes of the last reviewAttemptsKept finished,
// then the ids alone of the reviewAttemptIDsKept before those
// (forgotten). Once an id leaves that too (overflowed), an id it does not
// hold may be one it received, so it can no longer say an attempt never
// arrived.
type reviewAttempts struct {
	mu         sync.Mutex
	byID       map[string]*reviewAttempt
	finished   []string
	forgotten  map[string]string // id to PR key
	forgetting []string
	overflowed bool
}

type reviewAttempt struct {
	id      string
	prKey   string
	done    chan struct{}
	outcome PRReviewSubmitOutcome
	result  SubmitPRReviewResult
	err     error
}

// begin records attempt id, or returns the one already recorded under it.
// An id whose outcome was dropped returns a finished attempt that says so,
// so a repeat posts nothing.
func (r *reviewAttempts) begin(id, prKey string) (*reviewAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prior, ok := r.byID[id]; ok {
		return prior, true
	}
	if forgottenPR, ok := r.forgotten[id]; ok {
		attempt := &reviewAttempt{id: id, prKey: forgottenPR, done: make(chan struct{}), err: errors.New("submit PR review: this attempt ran before and its outcome is no longer kept")}
		close(attempt.done)
		return attempt, true
	}
	if r.byID == nil {
		r.byID = make(map[string]*reviewAttempt)
	}
	attempt := &reviewAttempt{id: id, prKey: prKey, done: make(chan struct{})}
	r.byID[id] = attempt
	return attempt, false
}

func (r *reviewAttempts) finish(attempt *reviewAttempt, result SubmitPRReviewResult, err error) {
	attempt.result, attempt.err = result, err
	switch {
	case err == nil && result.Unconfirmed:
		attempt.outcome = PRReviewSubmitOutcome{State: "unconfirmed", Result: &result}
	case err == nil:
		attempt.outcome = PRReviewSubmitOutcome{State: "posted", Result: &result}
	default:
		code, message, ok := errorsx.PublicDetails(err)
		if !ok {
			message = "The review was not posted."
		}
		attempt.outcome = PRReviewSubmitOutcome{State: "failed", Code: code, Message: message}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = append(r.finished, attempt.id)
	for len(r.finished) > reviewAttemptsKept {
		dropped := r.finished[0]
		if r.forgotten == nil {
			r.forgotten = make(map[string]string)
		}
		r.forgotten[dropped] = r.byID[dropped].prKey
		r.forgetting = append(r.forgetting, dropped)
		delete(r.byID, dropped)
		r.finished = r.finished[1:]
	}
	for len(r.forgetting) > reviewAttemptIDsKept {
		delete(r.forgotten, r.forgetting[0])
		r.forgetting = r.forgetting[1:]
		r.overflowed = true
	}
	close(attempt.done)
}

// find returns attempt id to prKey, or reports it forgotten: received and
// its outcome dropped, or not held once ids have overflowed, so it may
// have been received.
func (r *reviewAttempts) find(id, prKey string) (attempt *reviewAttempt, forgotten bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if attempt, ok := r.byID[id]; ok {
		if attempt.prKey == prKey {
			return attempt, false
		}
		return nil, false
	}
	if forgottenPR, ok := r.forgotten[id]; ok {
		return nil, forgottenPR == prKey
	}
	return nil, r.overflowed
}

// wait answers a repeated attempt with the first one's outcome.
func (attempt *reviewAttempt) wait(ctx context.Context) (SubmitPRReviewResult, error) {
	select {
	case <-attempt.done:
		return attempt.result, attempt.err
	case <-ctx.Done():
		return SubmitPRReviewResult{}, ctx.Err()
	}
}

// prDraftSource is the comment source of pr's pr-scope drafts; the
// frontend's prSourceKey builds the same string.
func prDraftSource(pr gitops.PRReference) string { return "pr:" + pr.Key() }

// postedReviewDraftsOf are the drafts a review that landed carried, as the
// forge received them. File-level comments post one by one after the
// review and stop at the first failure, so only the first
// PostedFileComments of them made it up. A draft with no line to anchor
// posts nothing and settles with the review.
func postedReviewDraftsOf(drafts []store.DiffReviewComment, result SubmitPRReviewResult) []store.PostedDiffReviewDraft {
	fileLevel := 0
	out := make([]store.PostedDiffReviewDraft, 0, len(drafts))
	for _, draft := range drafts {
		if draft.Side == "file" {
			fileLevel++
			if result.PartialFailurePath != "" && fileLevel > result.PostedFileComments {
				continue
			}
		}
		out = append(out, store.PostedDiffReviewDraftOf(draft))
	}
	return out
}

// reviewLineCommentForDraft is a draft as the forge's review comment: a
// file comment; an unchanged line, which a draft on either half or the
// unified row saves with both numbers, on the right at its new number with
// its old number beside it; a deleted line on the left at its old number;
// an added line on the right at its new number. A context draft missing a
// number is not sent.
func reviewLineCommentForDraft(draft store.DiffReviewComment) (gitops.ReviewLineComment, bool) {
	line := func(n int) *int { return &n }
	switch {
	case draft.Side == "file":
		return gitops.ReviewLineComment{Path: draft.FilePath, Body: draft.Body, Side: "file"}, true
	case draft.OldLine > 0 && draft.NewLine > 0:
		return gitops.ReviewLineComment{Path: draft.FilePath, Body: draft.Body, Line: line(draft.NewLine), OldLine: line(draft.OldLine), Side: "right"}, true
	case draft.Side == "old" && draft.OldLine > 0:
		return gitops.ReviewLineComment{Path: draft.FilePath, Body: draft.Body, Line: line(draft.OldLine), Side: "left"}, true
	case draft.Side == "new" && draft.NewLine > 0:
		return gitops.ReviewLineComment{Path: draft.FilePath, Body: draft.Body, Line: line(draft.NewLine), Side: "right"}, true
	}
	return gitops.ReviewLineComment{}, false
}

func (a *App) reviewSubmitLocks() *keyedlock.Registry {
	a.reviewSubmitsOnce.Do(func() { a.reviewSubmits = keyedlock.New() })
	return a.reviewSubmits
}

// reconcilePostedReviewDrafts marks the drafts of owner's reviews that
// posted but were not marked, and forgets each batch it marked (a draft
// edited since stays a draft). One it still cannot mark stays held, and
// its error says so.
func (a *App) reconcilePostedReviewDrafts(owner store.DiffReviewOwner) error {
	for _, batch := range a.postedReviewDrafts.held(owner) {
		if _, err := a.store.MarkPostedDiffReviewDraftsSent(owner, string(store.DiffReviewScopePR), batch.sourceKey, batch.drafts, batch.sentAt, batch.sentTurnID); err != nil {
			return errorsx.Public(ReviewDraftsUnmarkedCode, "These comments were posted by an earlier review and are being marked sent, so this review was not posted.", err)
		}
		a.postedReviewDrafts.forget(owner, batch)
		if owner.ThreadID != "" {
			a.announceDiffCommentsChanged(owner.ThreadID, string(store.DiffReviewScopePR), batch.sourceKey)
		}
	}
	return nil
}

// postedReviewBatch is one posted review's drafts that were not marked.
type postedReviewBatch struct {
	sourceKey  string
	sentTurnID string
	sentAt     int64
	drafts     []store.PostedDiffReviewDraft
}

// postedReviewDrafts holds, per owner, the batches of drafts a review
// posted that are not marked sent yet. In memory only: see the contract
// above for what a process exit leaves.
type postedReviewDrafts struct {
	mu      sync.Mutex
	byOwner map[store.DiffReviewOwner][]*postedReviewBatch
}

func (p *postedReviewDrafts) hold(owner store.DiffReviewOwner, batch postedReviewBatch) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byOwner == nil {
		p.byOwner = make(map[store.DiffReviewOwner][]*postedReviewBatch)
	}
	p.byOwner[owner] = append(p.byOwner[owner], &batch)
}

func (p *postedReviewDrafts) held(owner store.DiffReviewOwner) []*postedReviewBatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.byOwner[owner])
}

func (p *postedReviewDrafts) forget(owner store.DiffReviewOwner, batch *postedReviewBatch) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := slices.DeleteFunc(p.byOwner[owner], func(each *postedReviewBatch) bool { return each == batch })
	if len(kept) == 0 {
		delete(p.byOwner, owner)
		return
	}
	p.byOwner[owner] = kept
}

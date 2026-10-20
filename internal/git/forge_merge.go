package git

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// mergeHeadSHAPattern admits the full or abbreviated hex sha a forge
// reports as a head.
var mergeHeadSHAPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Merge readiness, merge options and forge events: the normalized shapes
// the review pane's Merge section and Conversation feed render, filled by
// each forge's ReadPR. The vocabularies here are the whole set the UI
// branches on; a forge value with no entry maps to the nearest kind with
// the forge's own text in Text. See docs/specs/pull-requests.md.

// MergeReadiness.State values.
const (
	MergeStateReady    = "ready"
	MergeStateBlocked  = "blocked"
	MergeStateChecking = "checking"
	MergeStateDraft    = "draft"
	MergeStateMerged   = "merged"
	MergeStateClosed   = "closed"
)

// MergeBlocker.Kind values. The UI pairs each with its fix: approvals
// offers Approve, checks opens the pipeline, threads jumps to the first
// unresolved thread, conflicts opens the conflict view, rebase offers
// Update branch, draft offers Mark ready; rules, changes_requested and
// other show the text alone.
const (
	BlockerApprovals        = "approvals"
	BlockerChecks           = "checks"
	BlockerThreads          = "threads"
	BlockerConflicts        = "conflicts"
	BlockerRebase           = "rebase"
	BlockerDraft            = "draft"
	BlockerRules            = "rules"
	BlockerChangesRequested = "changes_requested"
	BlockerOther            = "other"
)

// MergeMethod values, the forge-neutral names. GitLab's fast-forward
// method is MergeMethodRebase.
const (
	MergeMethodMerge  = "merge"
	MergeMethodSquash = "squash"
	MergeMethodRebase = "rebase"
)

// MergeReadiness is whether the forge would merge the PR now, and what
// stands in the way when it would not. Blockers is in the forge's order:
// empty when State is ready, checking, merged or closed; a draft leads
// with its draft blocker (the fix is Mark ready) and then what would block
// once ready. Settled lists the checks that pass (approvals met,
// pipeline passed, no conflicts) so the section has rows in every state.
type MergeReadiness struct {
	State    string         `json:"state"`
	Blockers []MergeBlocker `json:"blockers"`
	Settled  []MergeBlocker `json:"settled"`
}

// MergeBlocker is one condition of the merge. Text is the sentence the row
// shows ("Needs 1 more approval"), Detail the smaller line under it
// ("Randy Murphy approved · 2 required"), URL the forge page for the
// condition when one exists (a pipeline, a protection rule).
type MergeBlocker struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Detail string `json:"detail,omitempty"`
	URL    string `json:"url,omitempty"`
}

// MergeOptions is what the project allows and what the viewer may do,
// read with the PR's detail. Methods lists the allowed merge methods with
// the project's default first; empty when the viewer cannot read the
// project's settings, in which case the UI offers MergeMethodMerge alone.
type MergeOptions struct {
	Methods []string `json:"methods"`
	// DeleteBranchOffered is whether the menu shows the delete-branch
	// toggle: false for a fork's branch or when the project always deletes
	// it; DeleteBranchDefault is the toggle's initial state.
	DeleteBranchOffered bool `json:"deleteBranchOffered"`
	DeleteBranchDefault bool `json:"deleteBranchDefault"`
	// AutoMergeAvailable is whether the forge offers merging when the
	// checks pass; AutoMerge is the armed request, nil when none.
	AutoMergeAvailable bool              `json:"autoMergeAvailable"`
	AutoMerge          *AutoMergeRequest `json:"autoMerge,omitempty"`
	// ViewerCanMerge is whether the forge would let this login merge;
	// ViewerCanApprove whether it accepts an approval from this login
	// (GitHub refuses the author; GitLab's user_can_approve);
	// ViewerApproved whether the login's approval currently stands.
	ViewerCanMerge   bool `json:"viewerCanMerge"`
	ViewerCanApprove bool `json:"viewerCanApprove"`
	ViewerApproved   bool `json:"viewerApproved"`
	// CannotApproveReason explains an absent Approve button ("Authors
	// cannot approve their own pull request").
	CannotApproveReason string `json:"cannotApproveReason,omitempty"`
	// CannotMergeReason explains a greyed Merge now ("You do not have
	// merge access to this project").
	CannotMergeReason string `json:"cannotMergeReason,omitempty"`
}

// AutoMergeRequest is an armed merge-when-ready: who set it, when, with
// which method, and the forge's own wording of what it waits for.
type AutoMergeRequest struct {
	Method    string `json:"method"`
	ByLogin   string `json:"byLogin"`
	ByName    string `json:"byName,omitempty"`
	At        string `json:"at"`
	WaitsFor  string `json:"waitsFor"`
	CanCancel bool   `json:"canCancel"`
}

// ReviewEvent.Kind values: the forge activity the Conversation feed shows
// beside threads, verdicts and pushes.
const (
	EventApproved        = "approved"
	EventUnapproved      = "unapproved"
	EventReviewDismissed = "review_dismissed"
	EventMerged          = "merged"
	EventClosed          = "closed"
	EventReopened        = "reopened"
	EventMarkedDraft     = "marked_draft"
	EventMarkedReady     = "marked_ready"
	EventAutoMergeSet    = "auto_merge_set"
	EventAutoMergeCancel = "auto_merge_canceled"
	EventForcePushed     = "force_pushed"
	EventRebased         = "rebased"
	EventReviewRequested = "review_requested"
	EventBaseChanged     = "base_changed"
)

// ReviewEvent is one forge activity row. ID is unique within the PR and
// stable across reads (a GitLab note id, a GitHub timeline node id). At is
// RFC 3339. Text is the forge's own phrasing when it adds to the kind
// ("enabled an automatic merge when the pipeline succeeds"), else empty.
// Ref names what the event concerns when the kind has one: the review
// author for review_dismissed, the reviewer for review_requested, the
// new base for base_changed.
type ReviewEvent struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ActorLogin string `json:"actorLogin"`
	ActorName  string `json:"actorName,omitempty"`
	At         string `json:"at"`
	Text       string `json:"text,omitempty"`
	Ref        string `json:"ref,omitempty"`
}

// MergeRequest is one merge action. Method must be one of the PR's
// MergeOptions.Methods; HeadSHA is the head the caller saw and the forge
// refuses the merge when the branch moved past it. AutoMerge arms the
// merge for when the checks pass instead of merging now.
type MergeRequest struct {
	Method       string `json:"method"`
	HeadSHA      string `json:"headSHA"`
	DeleteBranch bool   `json:"deleteBranch"`
	AutoMerge    bool   `json:"autoMerge"`
}

// Validate checks the request's shape; forge-specific refusals come from
// the forge.
func (r MergeRequest) Validate() error {
	switch r.Method {
	case MergeMethodMerge, MergeMethodSquash, MergeMethodRebase:
	default:
		return fmt.Errorf("unsupported merge method %q", r.Method)
	}
	if !mergeHeadSHAPattern.MatchString(strings.TrimSpace(r.HeadSHA)) {
		return errors.New("merge requires the head commit sha the caller saw")
	}
	return nil
}

// MergeOutcome is what the forge did. Merged is true when the PR is
// merged now; AutoMerge when the forge armed the merge instead (a merge
// request that returned the PR still open, or an auto-merge request that
// succeeded). A forge that merges asynchronously answers neither and the
// caller follows the PR's next poll.
type MergeOutcome struct {
	Merged    bool   `json:"merged"`
	AutoMerge bool   `json:"autoMerge"`
	SHA       string `json:"sha,omitempty"`
}

// ForgeMerger is the merge-action half of Forge: approvals, merging,
// auto-merge and branch updates. Every method takes an interactive
// context (the caller marks it) and the PR's reference.
type ForgeMerger interface {
	// Approve records the viewer's approval of the PR at headSHA. GitLab
	// posts an approval; GitHub submits an approving review.
	Approve(ctx context.Context, ref PRReference, headSHA string) error
	// Unapprove withdraws the viewer's own standing approval: GitHub
	// dismisses the viewer's approving review (found on the forge, never
	// named by the caller), GitLab unapproves. Nothing of another login's
	// can be withdrawn through it.
	Unapprove(ctx context.Context, ref PRReference) error
	// Merge merges now, or arms auto-merge when req.AutoMerge.
	Merge(ctx context.Context, ref PRReference, req MergeRequest) (MergeOutcome, error)
	// CancelAutoMerge disarms an armed auto-merge.
	CancelAutoMerge(ctx context.Context, ref PRReference) error
	// UpdateBranch brings the head up to date with the base: GitLab
	// rebases, GitHub merges the base in or rebases per the repository.
	// The forge runs it asynchronously; the next poll shows the result.
	UpdateBranch(ctx context.Context, ref PRReference, headSHA string) error
	// MarkReady takes the PR out of draft.
	MarkReady(ctx context.Context, ref PRReference) error
}

func (nullForge) Approve(context.Context, PRReference, string) error { return ErrUnsupportedForge }
func (nullForge) Unapprove(context.Context, PRReference) error       { return ErrUnsupportedForge }
func (nullForge) Merge(context.Context, PRReference, MergeRequest) (MergeOutcome, error) {
	return MergeOutcome{}, ErrUnsupportedForge
}
func (nullForge) CancelAutoMerge(context.Context, PRReference) error      { return ErrUnsupportedForge }
func (nullForge) UpdateBranch(context.Context, PRReference, string) error { return ErrUnsupportedForge }
func (nullForge) MarkReady(context.Context, PRReference) error            { return ErrUnsupportedForge }

// ApprovePR records the caller's approval (see ForgeMerger.Approve).
func (c *Core) ApprovePR(ctx context.Context, ref PRReference, headSHA string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).Approve(ctx, ref, headSHA)
}

// UnapprovePR withdraws the caller's approval (see ForgeMerger.Unapprove).
func (c *Core) UnapprovePR(ctx context.Context, ref PRReference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).Unapprove(ctx, ref)
}

// MergePR merges now or arms auto-merge (see ForgeMerger.Merge).
func (c *Core) MergePR(ctx context.Context, ref PRReference, req MergeRequest) (MergeOutcome, error) {
	if err := ref.Validate(); err != nil {
		return MergeOutcome{}, err
	}
	if err := req.Validate(); err != nil {
		return MergeOutcome{}, err
	}
	return c.ForgeByID(ref.Forge).Merge(ctx, ref, req)
}

// CancelPRAutoMerge disarms auto-merge (see ForgeMerger.CancelAutoMerge).
func (c *Core) CancelPRAutoMerge(ctx context.Context, ref PRReference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).CancelAutoMerge(ctx, ref)
}

// UpdatePRBranch brings the head up to date with the base (see
// ForgeMerger.UpdateBranch).
func (c *Core) UpdatePRBranch(ctx context.Context, ref PRReference, headSHA string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).UpdateBranch(ctx, ref, headSHA)
}

// MarkPRReady takes the PR out of draft (see ForgeMerger.MarkReady).
func (c *Core) MarkPRReady(ctx context.Context, ref PRReference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).MarkReady(ctx, ref)
}

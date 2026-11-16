package forgefake

import (
	"fmt"
	"strings"
)

// GitHub's merge queue. A repository whose merge.mergeQueue is set
// merges its pulls only through the queue: every pull answers
// isMergeQueueEnabled, mergePullRequest is refused with GitHub's rule
// violation, enqueuePullRequest adds a pull that could merge now (its
// merge state CLEAN, UNSTABLE or HAS_HOOKS, as for mergePullRequest),
// dequeuePullRequest takes a queued one out, and auto-merge (join when
// ready) is offered whatever the repository's Allow auto-merge. A queued
// pull (Pull.Queued) answers isInMergeQueue and mergeQueueEntry; the fake
// never merges the queue on its own.

// QueueEntry is a pull's place in its base's merge queue. Position is
// GitHub's number (1 is next, as seeded), State GitHub's
// MergeQueueEntryState (QUEUED by default), By the login that queued it
// (the pull's author by default) and At when (now by default).
type QueueEntry struct {
	Position int    `json:"position,omitempty"`
	State    string `json:"state,omitempty"`
	By       string `json:"by,omitempty"`
	At       string `json:"at,omitempty"`
}

// githubMergeQueueRule is GitHub's refusal of a direct merge on a base
// that requires its merge queue.
const githubMergeQueueRule = "Repository rule violations found\n\nChanges must be made through the merge queue\n\n"

var githubQueueStates = map[string]bool{"QUEUED": true, "AWAITING_CHECKS": true, "MERGEABLE": true, "UNMERGEABLE": true, "LOCKED": true}

func (q *QueueEntry) normalize(author string) error {
	q.State = strings.ToUpper(defaultString(q.State, "QUEUED"))
	if !githubQueueStates[q.State] {
		return fmt.Errorf("queued state %q is not a MergeQueueEntryState", q.State)
	}
	if q.Position < 0 {
		return fmt.Errorf("queued position %d is negative", q.Position)
	}
	q.By = defaultString(q.By, author)
	q.At = defaultString(q.At, githubNow())
	return nil
}

// githubQueueFields answers a pull's merge queue fields.
func githubQueueFields(r *Repo, p *Pull) map[string]any {
	var entry any
	if q := p.Queued; q != nil {
		entry = map[string]any{"position": q.Position, "state": q.State, "enqueuedAt": q.At, "enqueuer": githubActor(q.By, "")}
	}
	var queue any
	if r.Merge.MergeQueue {
		size := 0
		for i := range r.Pulls {
			if r.Pulls[i].Queued != nil && r.Pulls[i].BaseRef == p.BaseRef {
				size++
			}
		}
		queue = map[string]any{"entries": map[string]any{"totalCount": size}}
	}
	return map[string]any{
		"isMergeQueueEnabled": r.Merge.MergeQueue,
		"isInMergeQueue":      p.Queued != nil,
		"mergeQueueEntry":     entry,
		"mergeQueue":          queue,
	}
}

// githubQueueTail is the position a newly queued pull takes.
func githubQueueTail(r *Repo) int {
	last := 0
	for i := range r.Pulls {
		if q := r.Pulls[i].Queued; q != nil && q.Position > last {
			last = q.Position
		}
	}
	return last + 1
}

// ghEnqueuePR adds a pull that could merge now to its base's queue, while
// its head is expectedHeadOid.
func ghEnqueuePR(e *Engine, c *call, v graphQLVars) response {
	const field = "enqueuePullRequest"
	head, err := v.str("expectedHeadOid")
	if err != nil {
		return unhandled("EnqueuePR: %v", err)
	}
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	switch {
	case p.State != "open":
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not open")
	case !r.Merge.MergeQueue:
		return graphQLMutationError(field, "UNPROCESSABLE", "Merge queue is not enabled for the base branch")
	case githubRoles[r.Merge.Permission] < githubRoles["WRITE"]:
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `EnqueuePullRequest`", e.viewer))
	case p.Queued != nil:
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is already in the merge queue")
	case head != p.HeadSHA:
		return graphQLMutationError(field, "UNPROCESSABLE", githubHeadModified)
	}
	switch githubMergeStateStatus(r, p, e.viewer) {
	case "CLEAN", "UNSTABLE", "HAS_HOOKS":
	default:
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not mergeable and cannot be added to the merge queue")
	}
	p.Queued = &QueueEntry{Position: githubQueueTail(r), State: "QUEUED", By: e.viewer, At: githubNow()}
	p.Readiness.AutoMerge = nil
	return graphQLData(map[string]any{field: map[string]any{"mergeQueueEntry": map[string]any{"id": fmt.Sprintf("MQE_ao%d", e.ids.next())}}})
}

// ghDequeuePR takes a queued pull out of its base's queue.
func ghDequeuePR(e *Engine, c *call, v graphQLVars) response {
	const field = "dequeuePullRequest"
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	switch {
	case p.Queued == nil:
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not in the merge queue")
	case githubRoles[r.Merge.Permission] < githubRoles["WRITE"]:
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `DequeuePullRequest`", e.viewer))
	}
	p.Queued = nil
	return graphQLData(map[string]any{field: map[string]any{"mergeQueueEntry": map[string]any{"id": fmt.Sprintf("MQE_ao%d", e.ids.next())}}})
}

// ghEnableQueuedAutoMerge is join-when-ready on a queue's base: auto-merge
// with no method (the queue's applies, here the repository's default)
// while the head is expectedHeadOid. A pull that could merge now joins
// the queue at once, as gh relies on (it queues through this mutation);
// any other is armed.
func ghEnableQueuedAutoMerge(e *Engine, c *call, v graphQLVars) response {
	const field = "enablePullRequestAutoMerge"
	head, err := v.str("expectedHeadOid")
	if err != nil {
		return unhandled("EnableQueuedAutoMerge: %v", err)
	}
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	switch {
	case p.State != "open":
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not open")
	case !r.Merge.AutoMergeAllowed && !r.Merge.MergeQueue:
		// GitHub takes a method-less auto-merge only where a queue
		// supplies the method; the fake answers the rest as a disallowed
		// auto-merge.
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request Auto merge is not allowed for this repository")
	case githubRoles[r.Merge.Permission] < githubRoles["WRITE"]:
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `EnablePullRequestAutoMerge`", e.viewer))
	case p.Readiness.AutoMerge != nil:
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request Auto merge is already enabled")
	case head != p.HeadSHA:
		return graphQLMutationError(field, "UNPROCESSABLE", githubHeadModified)
	}
	switch githubMergeStateStatus(r, p, e.viewer) {
	case "CLEAN", "UNSTABLE", "HAS_HOOKS":
		if !r.Merge.MergeQueue {
			return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is in clean status")
		}
		p.Queued = &QueueEntry{Position: githubQueueTail(r), State: "QUEUED", By: e.viewer, At: githubNow()}
		return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{
			"isInMergeQueue": true, "autoMergeRequest": nil,
		}}})
	case "DIRTY":
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request has merge conflicts")
	}
	now := githubNow()
	method := r.Merge.DefaultMethod
	p.Readiness.AutoMerge = &AutoMerge{Method: method, By: e.viewer, At: now}
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "auto_merge_set", Actor: e.viewer, At: now, Method: method})
	return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{
		"isInMergeQueue": false, "autoMergeRequest": map[string]any{"enabledAt": now},
	}}})
}

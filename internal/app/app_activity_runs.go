package app

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// On-demand activity-run members (docs/architecture/
// timeline-window-pages.md §3). A history page ships a window of each run
// and a stub for the rest; this is how the pane mounts past that window,
// jumps to a member the page did not ship, or refreshes a stub it marked
// dirty.

// ActivityRunStaleCode is the wire code a caller whose picture of the run
// has gone stale sees. The store refuses such a call outright — every
// count the pane would fold into its header and its held window depends
// on the span being real — and the pane follows the refusal with a window
// reload. A code rather than a sentence because the pane branches on it,
// and a non-loopback client is told only the public code and message.
const ActivityRunStaleCode = "activity_run_stale"

const activityRunStaleMessage = "This activity run changed while it was loading."

// ActivityRunMembersRequest is the JSON request body for
// ListActivityRunMembers: the store's request plus the shape the caller
// wants its rows in.
type ActivityRunMembersRequest struct {
	Selection store.TimelineSelection `json:"selection"`
	// RunFirstItemID identifies the run. The call is refused when the run
	// containing it no longer starts there.
	RunFirstItemID string `json:"runFirstItemId"`
	// LoadedFirstItemID / LoadedLastItemID bound the members the caller
	// already holds. Both "" when it holds none.
	LoadedFirstItemID string `json:"loadedFirstItemId,omitempty"`
	LoadedLastItemID  string `json:"loadedLastItemId,omitempty"`
	// Direction is "before", "after" or "around": extend the span older,
	// extend it newer, or replace it centered on AroundItemID.
	Direction string `json:"direction"`
	// AroundItemID is the member "around" centers on.
	AroundItemID string `json:"aroundItemId,omitempty"`
	// Limit caps the members returned. 0 returns none and refreshes the
	// stub for the span the caller already holds.
	Limit int `json:"limit,omitempty"`
	// Shape applies as it does to a page: InlinePreviews projects the
	// rows, MaxBytes bounds them. RunWindowRows has no meaning here — the
	// caller names the count it wants with Limit.
	Shape PageShape `json:"shape"`
}

// storeRequest is the request with the fields the store owns, at a given
// member limit. The limit is a parameter because the byte trim asks the
// store the same question again with a smaller one.
func (r ActivityRunMembersRequest) storeRequest(limit int) store.ActivityRunMembersRequest {
	return store.ActivityRunMembersRequest{
		Selection:         r.Selection,
		RunFirstItemID:    r.RunFirstItemID,
		LoadedFirstItemID: r.LoadedFirstItemID,
		LoadedLastItemID:  r.LoadedLastItemID,
		Direction:         r.Direction,
		AroundItemID:      r.AroundItemID,
		Limit:             limit,
	}
}

// ListActivityRunMembers returns up to `Limit` members of one run
// adjacent to the caller's loaded span, plus the stub describing the run
// for the span the caller holds AFTER this call. Limit 0 refreshes the
// stub only.
//
// The rows are projected and bounded by the caller's byte ceiling like
// any page. The store folds dropped rows into the stub it already computed,
// keeping the returned span exact without walking the whole run twice.
//
//ao:scope threads:read
func (a *App) ListActivityRunMembers(ctx context.Context, threadID string, req ActivityRunMembersRequest) (store.ActivityRunMembers, error) {
	if err := a.store.CheckForkReady(threadID); err != nil {
		return store.ActivityRunMembers{}, err
	}
	shape := req.Shape.normalize()
	members, err := a.store.ListActivityRunMembers(ctx, threadID, req.storeRequest(req.Limit))
	if err != nil {
		return store.ActivityRunMembers{}, activityRunMembersError(err)
	}
	members.Items = itemwire.ProjectItems(slicesx.OrEmpty(members.Items), shape.InlinePreviews)

	admitted := admittedMemberLimit(members, req, shape.MaxBytes)
	if admitted >= len(members.Items) {
		return members, nil
	}
	from := admittedMemberWindow(members, req, admitted, len(members.Items))
	members, err = members.TrimFresh(from, from+admitted)
	if err != nil {
		return store.ActivityRunMembers{}, activityRunMembersError(err)
	}
	return members, nil
}

// activityRunMembersError keeps the store's diagnosis in the chain, and
// marks a stale refusal with the public code the pane branches on.
func activityRunMembersError(err error) error {
	wrapped := fmt.Errorf("list activity run members: %w", err)
	if errors.Is(err, store.ErrActivityRunStale) {
		return errorsx.Public(ActivityRunStaleCode, activityRunStaleMessage, wrapped)
	}
	return wrapped
}

// admittedMemberLimit is the largest member limit whose answer fits the
// caller's byte ceiling, or 1 when even one member does not: a response
// that mounts nothing would leave the boundary it was fetched for stuck
// forever, so an oversized member ships alone, exactly as an oversized
// page anchor does.
//
// It answers in terms of the LIMIT rather than a row range because the
// response is re-requested at that limit. The windows the store returns
// for successive limits nest — "before" takes the newest k it can,
// "after" the oldest, "around" a window centered on the same member — so
// cost is monotone in the limit and the largest one that fits can be
// found by walking down from the rows already in hand.
func admittedMemberLimit(members store.ActivityRunMembers, req ActivityRunMembersRequest, maxBytes int) int {
	items := members.Items
	if len(items) == 0 {
		return 0
	}
	costs := make([]int, len(items)+1)
	for i, item := range items {
		costs[i+1] = costs[i] + itemwire.EncodedBytes(item)
	}
	for limit := len(items); limit > 1; limit-- {
		lo := admittedMemberWindow(members, req, limit, len(items))
		if costs[lo+limit]-costs[lo] <= maxBytes {
			return limit
		}
	}
	return 1
}

// admittedMemberWindow is where the store's answer at `limit` starts
// within the rows it answered with at the larger limit.
func admittedMemberWindow(members store.ActivityRunMembers, req ActivityRunMembersRequest, limit, have int) int {
	if req.Direction != store.ActivityRunMembersAround {
		if req.Direction == store.ActivityRunMembersBefore {
			// Older-ward: the members nearest the caller's span are the
			// newest ones, so a smaller limit keeps the suffix.
			return have - limit
		}
		return 0
	}
	// "around" replaces the span, so the member it centers on is what a
	// smaller window keeps: mirror the store's centering rather than
	// growing outward independently, or the response and the stub would
	// describe different rows.
	center := indexOfItemID(members.Items, req.AroundItemID)
	if center < 0 {
		return 0
	}
	spanStart := members.Stub.UnshippedBefore
	start := min(max(spanStart+center-limit/2, 0), members.Stub.MemberCount-limit)
	return min(max(start-spanStart, 0), have-limit)
}

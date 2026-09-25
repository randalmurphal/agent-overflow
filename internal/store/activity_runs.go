package store

import (
	"sort"
)

// Activity runs on the server (docs/architecture/timeline-window-pages.md
// §1 and §4).
//
// A history page ships every prose row in its range, and for every run in
// its range a stub plus only the members that would mount. This file owns
// the two halves that decision rests on: which rows form a run, and what
// one run's unshipped members add up to.
//
// The membership rule and the group aggregate are the SAME rule the
// client applies in frontend/src/lib/utils/timelineRail.ts,
// activityRunGrouping.ts and components/chat/activityRunSummary.ts. Both
// sides run testdata/activity_run_vectors.json (byte-identical copy at
// frontend/src/test/fixtures/activityRunVectors.json), so the rule cannot
// drift: a server stub and a client-side classification of the same rows
// have to add up to the same header.

// railRowKinds are the item kinds that sit on the timeline rail
// (timelineRail.ts RAIL_LEAF_KINDS). Subagent and read group containers
// are a projection of these same rows, so the physical rule needs only
// the leaf kinds.
var railRowKinds = map[string]struct{}{
	"tool_call":            {},
	"tool_completion":      {},
	"terminal_interaction": {},
	"thinking":             {},
}

// railExemptPayloadKind is the one payload kind that takes a rail row off
// the rail (timelineRail.ts RAIL_EXEMPT_PAYLOAD_KINDS): a proposed plan
// renders as a full-width card, so it breaks a run rather than joining
// it. Payload kind is only known after the payload join, which is why
// classification happens in Go over the narrow scan and never in SQL.
const railExemptPayloadKind = "proposed_plan"

const (
	notificationKind   = "notification"
	toolCompletionKind = "tool_completion"
)

// isRailRow reports rail participation, the run-membership predicate for
// every row except an absorbed notification.
func (r activityScanRow) isRailRow() bool {
	if _, ok := railRowKinds[r.Kind]; !ok {
		return false
	}
	return r.PayloadKind != railExemptPayloadKind
}

// isBell reports a notification an activity run can absorb.
func (r activityScanRow) isBell() bool {
	return r.Kind == notificationKind
}

// isFailedStatus and isRunningStatus are §4's status rules.
// `declined` is a user decision, not a failure.
func (r activityScanRow) isFailedStatus() bool {
	return r.Status == "errored" || r.Status == "killed"
}

func (r activityScanRow) isRunningStatus() bool {
	return r.Status == "running" || r.Status == "streaming"
}

// completionOf names the launch this row completes, "" for any other row.
func (r activityScanRow) completionOf() string {
	if r.Kind != toolCompletionKind {
		return ""
	}
	return r.CompletionOf
}

// endsLaunch reports a completion that ends its launch: every completion
// but a parked stop, which pauses a background agent's run
// (agent_stops.go). Any completion pairs with its launch for the counts;
// only an ending one supersedes the launch's status (§4).
func (r activityScanRow) endsLaunch() bool {
	return r.completionOf() != "" && r.Status != ItemStatusParked
}

func (r activityScanRow) groupKey() ActivityRunGroupKey {
	return ActivityRunGroupKey{Kind: r.Kind, ToolName: r.ToolName, MCP: r.MCP}
}

// ActivityRunGroupKey identifies one presented group of run members.
// Presentation — label, icon, provider aliasing, MCP family — stays in
// TypeScript; this is the raw identity it builds from, exactly as it
// builds it from a loaded Item.
type ActivityRunGroupKey struct {
	Kind     string `json:"kind"`
	ToolName string `json:"toolName"`
	// MCP is `json_extract(items.meta, '$.mcp')`, the `{server, tool}`
	// object both providers stamp on a tool a server served, as JSON
	// text. "" for native tools.
	MCP string `json:"mcp"`
}

// ActivityRunGroup is one group's contribution to a run header.
type ActivityRunGroup struct {
	ActivityRunGroupKey
	// Rows is the sum of DISPLAY rows (§4), not the member count: one
	// file-change call standing for four files counts four, and a
	// completion paired with a launch in the same run counts zero.
	Rows int `json:"rows"`
}

// ActivityRunStub describes one activity run to a client that holds only
// part of it. Every physical row of the run is either shipped in the
// page's Items or counted here, which is what lets a client fold the stub
// into a held-window description (§5) and drop rows without asking the
// server (§6).
type ActivityRunStub struct {
	// FirstItemID and LastItemID are the run's physical edges. A run is
	// identified by FirstItemID: runs grow only at their newer end, so
	// the first member is stable for the life of the window.
	FirstItemID string `json:"firstItemId"`
	LastItemID  string `json:"lastItemId"`
	// FirstTurnIndex/FirstItemIndex and LastTurnIndex/LastItemIndex are
	// the coordinates of those edges. A run is contiguous over the rows a
	// page returns, so a client can decide from these alone whether a
	// top-level row it does not hold is a member: it is exactly when its
	// coordinates fall between the edges (§6 jumps).
	FirstTurnIndex int `json:"firstTurnIndex"`
	FirstItemIndex int `json:"firstItemIndex"`
	LastTurnIndex  int `json:"lastTurnIndex"`
	LastItemIndex  int `json:"lastItemIndex"`
	// MemberCount counts every physical member, shipped or not.
	MemberCount int `json:"memberCount"`
	// LoadedFirstItemID and LoadedLastItemID bound the shipped span, and
	// are "" when the page ships no member of this run.
	LoadedFirstItemID string `json:"loadedFirstItemId"`
	LoadedLastItemID  string `json:"loadedLastItemId"`
	// UnshippedBefore and UnshippedAfter count the members outside the
	// shipped span on each side. With an empty shipped span every member
	// counts as UnshippedBefore: the run reads as entirely earlier
	// history, which is the direction a first fetch takes.
	UnshippedBefore int `json:"unshippedBefore"`
	UnshippedAfter  int `json:"unshippedAfter"`
	// UnshippedDigest is WindowDigest over every unshipped member, so a
	// client can XOR it into the window digest it holds (§5).
	UnshippedDigest string `json:"unshippedDigest"`
	// UnshippedGroups is the header contribution of the unshipped
	// members, sorted by (kind, toolName, mcp) so two reads of the same
	// run produce the same bytes.
	UnshippedGroups []ActivityRunGroup `json:"unshippedGroups"`
	// UnshippedPairedLaunchIDs lists unshipped members a shipped
	// completion names, sorted by id. The client holds such a completion
	// without its launch; this is how it applies §4's pairing rule to it
	// and counts it zero.
	UnshippedPairedLaunchIDs []string `json:"unshippedPairedLaunchIds"`
	// ShippedSupersededLaunchIDs lists shipped members whose ENDING
	// completion (activityScanRow.endsLaunch) in this run is NOT shipped,
	// sorted by id. The client holds such a launch without the completion
	// that ends it; this is how it knows the launch's status is
	// superseded (§4) instead of reading it live. Together the two lists
	// cover every launch/completion pair the shipped span splits.
	ShippedSupersededLaunchIDs []string `json:"shippedSupersededLaunchIds"`
	// UnshippedFailed reports an unshipped member the header must show as
	// failed: errored or killed, and not superseded by an ending
	// completion in the same run.
	UnshippedFailed bool `json:"unshippedFailed"`
	// RunningBefore and RunningAfter name the newest running member on
	// each side of the shipped span, or nil.
	RunningBefore *ActivityRunGroupKey `json:"runningBefore"`
	RunningAfter  *ActivityRunGroupKey `json:"runningAfter"`

	// unshippedDigestBits is UnshippedDigest before rendering. Both are
	// written by setUnshippedDigest and only there, so the fold does not
	// have to parse a string this package just produced.
	unshippedDigestBits uint64

	// first and last are the run's edges as timeline coordinates. A page
	// edge may land on an unshipped member, so neither the composer nor
	// TrimShipped can rebuild a cursor from the shipped Items alone.
	first, last TimelineCursor
	// shipped is, in page order, what folding each shipped member back
	// into this stub would contribute. It is decided at compose time,
	// where the whole run is known: whether a completion supersedes a
	// launch's status is a fact about the run, not about the page, and
	// the byte trim (§2.2) cannot re-derive it from the rows it drops.
	shipped []activityFoldRow
}

// activityFoldRow is one shipped member's folded form: everything
// TrimShipped needs to move that member from Items into its stub.
type activityFoldRow struct {
	id  string
	rev int64
	key ActivityRunGroupKey
	// displayRows is 0 for a completion paired with a launch in the same
	// run, which is what makes the pairing rule survive the fold.
	displayRows int
	failed      bool
	running     bool
	// supersededByMember is true when an ending completion in this run
	// covers this row's status, so folding it may not report failed or
	// running.
	supersededByMember bool
	// completionOfMember names the member this row completes, "" when it
	// completes nothing in this run. endsMember is true when it ends that
	// member rather than parking it.
	completionOfMember string
	endsMember         bool
}

// buildActivityRunStub aggregates one run against the members the page
// ships. `rows` is every member in (turn_index, item_index) order;
// [shippedFrom, shippedTo) is the shipped span over it, and an empty
// range means the page ships none of them.
//
// Every per-member fact the page-level byte trim may later need is
// decided here, because here is where the whole run is in hand.
func buildActivityRunStub(rows []activityScanRow, shippedFrom, shippedTo int) ActivityRunStub {
	// Index only launch IDs named by completions. A long run may contain
	// thousands of other members, none of which pairing needs to retain.
	// The value is whether an ENDING completion names the launch.
	var completionTargets map[string]bool
	for _, row := range rows {
		if launch := row.completionOf(); launch != "" {
			if completionTargets == nil {
				completionTargets = make(map[string]bool)
			}
			completionTargets[launch] = completionTargets[launch] || row.endsLaunch()
		}
	}
	// A completion counts zero rows itself, and an ending one supersedes
	// its launch's status, only when the launch is a member of the SAME
	// run. The value is whether an ending completion supersedes it.
	var completedMembers map[string]bool
	for _, row := range rows {
		if ended, ok := completionTargets[row.ID]; ok {
			if completedMembers == nil {
				completedMembers = make(map[string]bool)
			}
			completedMembers[row.ID] = ended
		}
	}

	stub := ActivityRunStub{
		FirstItemID:                rows[0].ID,
		LastItemID:                 rows[len(rows)-1].ID,
		FirstTurnIndex:             rows[0].TurnIndex,
		FirstItemIndex:             rows[0].ItemIndex,
		LastTurnIndex:              rows[len(rows)-1].TurnIndex,
		LastItemIndex:              rows[len(rows)-1].ItemIndex,
		MemberCount:                len(rows),
		UnshippedGroups:            []ActivityRunGroup{},
		UnshippedPairedLaunchIDs:   []string{},
		ShippedSupersededLaunchIDs: []string{},
		first:                      rows[0].cursor(),
		last:                       rows[len(rows)-1].cursor(),
	}
	if shippedFrom < shippedTo {
		stub.LoadedFirstItemID = rows[shippedFrom].ID
		stub.LoadedLastItemID = rows[shippedTo-1].ID
		stub.shipped = make([]activityFoldRow, 0, shippedTo-shippedFrom)
	}

	groups := map[ActivityRunGroupKey]int{}
	var digest uint64
	// With no shipped span every member reads as earlier history, so the
	// whole run counts before it and the newest running member is the
	// "before" edge.
	emptySpan := shippedFrom >= shippedTo
	for i, row := range rows {
		fold := activityFoldRow{
			id:          row.ID,
			rev:         row.Rev,
			key:         row.groupKey(),
			displayRows: row.DisplayRows,
			failed:      row.isFailedStatus(),
			running:     row.isRunningStatus(),
		}
		fold.supersededByMember = completedMembers[row.ID]
		if launch := row.completionOf(); launch != "" {
			if _, ok := completedMembers[launch]; ok {
				fold.completionOfMember = launch
				fold.endsMember = row.endsLaunch()
				fold.displayRows = 0
			}
		}
		if i >= shippedFrom && i < shippedTo {
			stub.shipped = append(stub.shipped, fold)
			continue
		}
		digest ^= windowDigestRowHash(WindowDigestRow{ID: fold.id, Rev: fold.rev})
		if fold.displayRows > 0 {
			groups[fold.key] += fold.displayRows
		}
		beforeSpan := emptySpan || i < shippedFrom
		if !fold.supersededByMember {
			if fold.failed {
				stub.UnshippedFailed = true
			}
			if fold.running {
				key := fold.key
				if beforeSpan {
					stub.RunningBefore = &key
				} else {
					stub.RunningAfter = &key
				}
			}
		}
		if beforeSpan {
			stub.UnshippedBefore++
		} else {
			stub.UnshippedAfter++
		}
	}
	stub.setUnshippedDigest(digest)
	stub.UnshippedGroups = sortedActivityRunGroups(groups)
	stub.setLaunchLists()
	return stub
}

// setLaunchLists derives the two launch lists from the members the stub
// ships, the only thing they depend on. A completion of any status pairs
// with its launch for the counts, so an unshipped launch is paired while
// any completion of it is shipped. Only an ending completion supersedes
// the launch's status, so a shipped launch is superseded while its ending
// completion is a member the page does not ship. Deriving both from the
// shipped span, rather than moving ids between them fold by fold, holds
// for a launch with several completions: a background agent's parked
// stops and its ending one.
func (p *ActivityRunStub) setLaunchLists() {
	p.UnshippedPairedLaunchIDs = []string{}
	p.ShippedSupersededLaunchIDs = []string{}
	// Launch -> whether a shipped completion naming it ends it, for every
	// launch a shipped completion names.
	var named map[string]bool
	for _, fold := range p.shipped {
		if fold.completionOfMember == "" {
			continue
		}
		if named == nil {
			named = make(map[string]bool)
		}
		named[fold.completionOfMember] = named[fold.completionOfMember] || fold.endsMember
	}
	for _, fold := range p.shipped {
		endShipped, isNamed := named[fold.id]
		if fold.supersededByMember && !endShipped {
			p.ShippedSupersededLaunchIDs = append(p.ShippedSupersededLaunchIDs, fold.id)
		}
		if isNamed {
			delete(named, fold.id)
		}
	}
	for launch := range named {
		p.UnshippedPairedLaunchIDs = append(p.UnshippedPairedLaunchIDs, launch)
	}
	sort.Strings(p.UnshippedPairedLaunchIDs)
	sort.Strings(p.ShippedSupersededLaunchIDs)
}

// foldShippedMember moves one shipped member back out of the page and
// into the stub (§2.2). `older` says which side of the surviving shipped
// span the member sits on, which is what decides the running edge it may
// claim: an older-end fold is NEWER than everything already counted
// before the span, while a newer-end fold is OLDER than everything
// already counted after it. Callers fold the older side oldest-first and
// the newer side newest-first, so on both sides the newest running fold
// is the one that stands.
func (p *ActivityRunStub) foldShippedMember(fold activityFoldRow, older bool) {
	p.setUnshippedDigest(p.unshippedDigestBits ^
		windowDigestRowHash(WindowDigestRow{ID: fold.id, Rev: fold.rev}))
	if fold.displayRows > 0 {
		groups := map[ActivityRunGroupKey]int{}
		for _, group := range p.UnshippedGroups {
			groups[group.ActivityRunGroupKey] = group.Rows
		}
		groups[fold.key] += fold.displayRows
		p.UnshippedGroups = sortedActivityRunGroups(groups)
	}
	if !fold.supersededByMember {
		if fold.failed {
			p.UnshippedFailed = true
		}
		if fold.running {
			key := fold.key
			if older {
				p.RunningBefore = &key
			} else if p.RunningAfter == nil {
				p.RunningAfter = &key
			}
		}
	}
	if older {
		p.UnshippedBefore++
	} else {
		p.UnshippedAfter++
	}
}

// setUnshippedDigest is the only writer of the stub's digest, so the
// rendered hex and the bits the fold XORs into can never disagree.
func (p *ActivityRunStub) setUnshippedDigest(bits uint64) {
	p.unshippedDigestBits = bits
	p.UnshippedDigest = formatWindowDigest(bits)
}

func sortedActivityRunGroups(groups map[ActivityRunGroupKey]int) []ActivityRunGroup {
	out := make([]ActivityRunGroup, 0, len(groups))
	for key, rows := range groups {
		out = append(out, ActivityRunGroup{ActivityRunGroupKey: key, Rows: rows})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].ToolName != out[j].ToolName {
			return out[i].ToolName < out[j].ToolName
		}
		return out[i].MCP < out[j].MCP
	})
	return out
}

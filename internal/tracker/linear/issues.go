package linear

import (
	"context"
	"fmt"
	"time"
)

// Page sizes of the issue reads. A page of issues costs about 1,100
// points (issueFields); its answer carries the descriptions, so its body
// may run to issueBodyBytes. The single-issue read costs about 750: 50
// comments at 4, 50 children at 2.4, and 25 attachments and 25 relations
// each way.
const (
	issuePage      = 100
	issueBodyBytes = 32 << 20
	commentPage    = 100
	historyPage    = 100
)

// IssueOrder orders a list of issues.
type IssueOrder int

const (
	// OrderManual is the manual order of board columns and the backlog:
	// SortOrder, lowest first.
	OrderManual IssueOrder = iota
	// OrderPriority is by priority, urgent first, then PrioritySortOrder
	// within a priority.
	OrderPriority
	OrderUpdatedNewest
	OrderCreatedNewest
	// orderUpdatedOldest is the polling order.
	orderUpdatedOldest
)

// IssueQuery selects and orders issues.
type IssueQuery struct {
	Filter          IssueFilter
	Order           IssueOrder
	IncludeArchived bool
}

var opIssues = operation{name: "Issues", maxBody: issueBodyBytes, text: `query Issues($filter: IssueFilter, $sort: [IssueSortInput!], $includeArchived: Boolean, $first: Int!, $after: String) {
  page: issues(filter: $filter, sort: $sort, includeArchived: $includeArchived, first: $first, after: $after) {
    nodes { ...IssueFields } pageInfo { hasNextPage endCursor }
  }
}
` + issueFields}

type sortDirection struct {
	Order string `json:"order"`
}

// issueSort is one entry of the schema's IssueSortInput list.
type issueSort struct {
	Manual    *sortDirection `json:"manual,omitempty"`
	Priority  *sortDirection `json:"priority,omitempty"`
	UpdatedAt *sortDirection `json:"updatedAt,omitempty"`
	CreatedAt *sortDirection `json:"createdAt,omitempty"`
}

type issuesVars struct {
	pageVars
	Filter          *issueFilterInput `json:"filter,omitempty"`
	Sort            []issueSort       `json:"sort"`
	IncludeArchived bool              `json:"includeArchived,omitempty"`
}

func (o IssueOrder) sort() ([]issueSort, error) {
	asc, desc := &sortDirection{"Ascending"}, &sortDirection{"Descending"}
	switch o {
	case OrderManual:
		return []issueSort{{Manual: asc}}, nil
	case OrderPriority:
		return []issueSort{{Priority: asc}}, nil
	case OrderUpdatedNewest:
		return []issueSort{{UpdatedAt: desc}}, nil
	case OrderCreatedNewest:
		return []issueSort{{CreatedAt: desc}}, nil
	case orderUpdatedOldest:
		return []issueSort{{UpdatedAt: asc}}, nil
	}
	return nil, fmt.Errorf("unknown IssueOrder %d", o)
}

// ListIssues returns the page of issues q selects that follows after,
// the previous page's EndCursor (empty for the first). Pass the same q
// for every page of one walk.
func (c *Client) ListIssues(ctx context.Context, q IssueQuery, after string) (Page[Issue], error) {
	vars, err := issueVars(q, after)
	if err != nil {
		return Page[Issue]{}, invalid(opIssues.name, "%w", err)
	}
	return fetchPage(ctx, c, opIssues, vars, false, wireIssue.typed)
}

func issueVars(q IssueQuery, after string) (issuesVars, error) {
	filter, err := q.Filter.input()
	if err != nil {
		return issuesVars{}, err
	}
	sort, err := q.Order.sort()
	if err != nil {
		return issuesVars{}, err
	}
	return issuesVars{pageVars{First: issuePage, After: after}, filter, sort, q.IncludeArchived}, nil
}

// PollQuery selects the issues a sync loop polls.
type PollQuery struct {
	// TeamID limits the poll to one team; empty polls every team.
	TeamID string
	// Since is inclusive: issues updated at or after it. Zero reads every
	// issue.
	Since           time.Time
	IncludeArchived bool
}

// PollPage is a page of a poll and where the next poll resumes.
type PollPage struct {
	Page[Issue]
	// Resume is the Since to poll from next: the latest UpdatedAt read so
	// far, or the query's Since when the page is empty.
	Resume time.Time
}

// PollIssues returns the page after the cursor after of the issues
// updated at or after q.Since, oldest update first. Pass the same q for
// every page of one walk.
//
// Resume rule: after any page, the walk may stop and the next poll
// passes that page's Resume as Since. Pages run in update order, so
// every issue updated before Resume was delivered; Since is inclusive,
// so issues updated at Resume are delivered again, and the caller keeps
// an issue only when its UpdatedAt is newer than the copy it holds. An
// issue updated during a walk moves to the end of the order and is
// delivered by a later page or the next poll.
//
// Archived issues come only with IncludeArchived. Archiving and
// unarchiving update the issue, so a poll that includes archived issues
// sees both. A permanently deleted issue cannot be seen by polling; a
// sync loop finds one by reading the issue and getting ErrNotFound.
func (c *Client) PollIssues(ctx context.Context, q PollQuery, after string) (PollPage, error) {
	var filter IssueFilter
	if q.TeamID != "" {
		filter.Teams = []string{q.TeamID}
	}
	filter.UpdatedSince = q.Since
	vars, err := issueVars(IssueQuery{Filter: filter, Order: orderUpdatedOldest, IncludeArchived: q.IncludeArchived}, after)
	if err != nil {
		return PollPage{}, invalid(opIssues.name, "%w", err)
	}
	p, err := fetchPage(ctx, c, opIssues, vars, false, wireIssue.typed)
	if err != nil {
		return PollPage{}, err
	}
	out := PollPage{Page: p, Resume: q.Since}
	for _, issue := range p.Items {
		if issue.UpdatedAt.After(out.Resume) {
			out.Resume = issue.UpdatedAt
		}
	}
	return out, nil
}

var opIssue = operation{name: "Issue", text: `query Issue($id: String!) {
  issue(id: $id) {
    ...IssueFields
    parent { ...IssueRefFields }
    children(first: 50) { nodes { ...IssueRefFields } pageInfo { hasNextPage } }
    comments(first: 50) { nodes { ...CommentFields } pageInfo { hasNextPage endCursor } }
    attachments(first: 25) { nodes { ...AttachmentFields } pageInfo { hasNextPage } }
    relations(first: 25) { nodes { ...RelationFields } pageInfo { hasNextPage } }
    inverseRelations(first: 25) { nodes { ...RelationFields } pageInfo { hasNextPage } }
  }
}
` + issueFields + issueRefFields + commentFields + attachmentFields + relationFields}

type wireIssueDetail struct {
	wireIssue
	Parent           *wireIssueRef              `json:"parent"`
	Children         connection[wireIssueRef]   `json:"children"`
	Comments         connection[wireComment]    `json:"comments"`
	Attachments      connection[wireAttachment] `json:"attachments"`
	Relations        connection[wireRelation]   `json:"relations"`
	InverseRelations connection[wireRelation]   `json:"inverseRelations"`
}

type idVars struct {
	ID string `json:"id"`
}

// Issue returns one issue by its ID or its identifier, such as WEB-1,
// archived or not. A missing or invisible issue matches
// trackerapi.ErrNotFound.
func (c *Client) Issue(ctx context.Context, idOrIdentifier string) (*IssueDetail, error) {
	if idOrIdentifier == "" {
		return nil, invalid("Issue", "an issue ID or identifier is required")
	}
	var data struct {
		Issue *wireIssueDetail `json:"issue"`
	}
	usage, err := c.call(ctx, opIssue, idVars{idOrIdentifier}, &data)
	if err != nil {
		return nil, err
	}
	if data.Issue == nil {
		return nil, fmt.Errorf("linear %s: the answer holds no issue", opIssue.name)
	}
	w := data.Issue
	d := &IssueDetail{
		Issue:           w.typed(),
		Parent:          w.Parent.typedPtr(),
		Children:        page(w.Children, usage, wireIssueRef.typed).Items,
		MoreChildren:    w.Children.PageInfo.HasNextPage,
		Comments:        page(w.Comments, usage, wireComment.typed),
		Attachments:     page(w.Attachments, usage, wireAttachment.typed).Items,
		MoreAttachments: w.Attachments.PageInfo.HasNextPage,
		MoreRelations:   w.Relations.PageInfo.HasNextPage || w.InverseRelations.PageInfo.HasNextPage,
	}
	// The detail's parent selection shadows the fragment's parent { id }.
	if d.Parent != nil {
		d.ParentID = d.Parent.ID
	}
	for _, r := range append(w.Relations.Nodes, w.InverseRelations.Nodes...) {
		d.Relations = append(d.Relations, r.typed())
	}
	return d, nil
}

type issuePageVars struct {
	pageVars
	ID string `json:"id"`
}

var opIssueComments = operation{name: "IssueComments", text: `query IssueComments($id: String!, $first: Int!, $after: String) {
  node: issue(id: $id) {
    page: comments(first: $first, after: $after) { nodes { ...CommentFields } pageInfo { hasNextPage endCursor } }
  }
}
` + commentFields}

// IssueComments returns the page of an issue's comments after the cursor
// after, newest first (Linear's default order); replies carry their
// ParentID. Issue holds the first page and its EndCursor.
func (c *Client) IssueComments(ctx context.Context, issueID, after string) (Page[Comment], error) {
	if issueID == "" {
		return Page[Comment]{}, invalid("IssueComments", "an issue ID is required")
	}
	return fetchPage(ctx, c, opIssueComments, issuePageVars{pageVars{commentPage, after}, issueID}, true, wireComment.typed)
}

var opIssueHistory = operation{name: "IssueHistory", text: `query IssueHistory($id: String!, $first: Int!, $after: String) {
  node: issue(id: $id) {
    page: history(first: $first, after: $after) { nodes { ...HistoryFields } pageInfo { hasNextPage endCursor } }
  }
}
` + historyFields}

// IssueHistory returns the page of an issue's recorded changes after the
// cursor after, newest first (Linear's default order).
// Changes made in the first minutes after creation are recorded as part
// of the creation, not as entries.
func (c *Client) IssueHistory(ctx context.Context, issueID, after string) (Page[HistoryEntry], error) {
	if issueID == "" {
		return Page[HistoryEntry]{}, invalid("IssueHistory", "an issue ID is required")
	}
	return fetchPage(ctx, c, opIssueHistory, issuePageVars{pageVars{historyPage, after}, issueID}, true, wireHistory.typed)
}

var opIssueStateSpans = operation{name: "IssueStateSpans", text: `query IssueStateSpans($id: String!, $first: Int!, $after: String) {
  node: issue(id: $id) {
    page: stateHistory(first: $first, after: $after) { nodes { ...StateSpanFields } pageInfo { hasNextPage endCursor } }
  }
}
` + stateSpanFields}

// IssueStateSpans returns the page of periods an issue spent in each
// status after the cursor after, oldest first. Re-entering a status
// starts a new span.
func (c *Client) IssueStateSpans(ctx context.Context, issueID, after string) (Page[StateSpan], error) {
	if issueID == "" {
		return Page[StateSpan]{}, invalid("IssueStateSpans", "an issue ID is required")
	}
	return fetchPage(ctx, c, opIssueStateSpans, issuePageVars{pageVars{historyPage, after}, issueID}, true, wireStateSpan.typed)
}

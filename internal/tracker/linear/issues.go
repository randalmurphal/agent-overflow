package linear

import (
	"context"
	"fmt"
	"strings"
)

// Page sizes of the issue reads. Linear charges a page of issues by the
// issues it returns, about 4.5 points each with their labels (measured),
// not by the page size; its answer carries the descriptions, so its body
// may run to issueBodyBytes. The single-issue
// read costs about 900 by the documented rule, which Linear applies to
// nested connections: 50 comments at 4, 50 children at 2.4, 25
// attachments at 4 and 25 relations each way at 6. A relation page costs
// about 600.
const (
	issuePage      = 100
	issueBodyBytes = 32 << 20
	commentPage    = 100
	historyPage    = 100
	relationPage   = 100
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

// maxIssuePage is the largest page the package asks for. Linear's schema
// and pagination guide state no maximum; it served issues at 250
// (measured).
const maxIssuePage = 250

// IssueQuery selects and orders issues.
type IssueQuery struct {
	Filter          IssueFilter
	Order           IssueOrder
	IncludeArchived bool
	// Limit is the page size, 1 to 250; zero means 100. A page's cost
	// grows with it.
	Limit int
}

var opIssues = newQuery("Issues", "page", `query Issues($filter: IssueFilter, $sort: [IssueSortInput!], $includeArchived: Boolean, $first: Int!, $after: String) {
  page: issues(filter: $filter, sort: $sort, includeArchived: $includeArchived, first: $first, after: $after) {
    nodes { ...IssueFields } pageInfo { hasNextPage endCursor }
  }
}
`+issueFields).withMaxBody(issueBodyBytes)

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
	return fetchPage(ctx, c, opIssues, vars, wireIssue.typed)
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
	first, err := pageSize(q.Limit)
	if err != nil {
		return issuesVars{}, err
	}
	return issuesVars{pageVars{First: first, After: after}, filter, sort, q.IncludeArchived}, nil
}

// pageSize is the first argument of an issue page of limit; zero is
// issuePage.
func pageSize(limit int) (int, error) {
	switch {
	case limit == 0:
		return issuePage, nil
	case limit < 0 || limit > maxIssuePage:
		return 0, fmt.Errorf("a page holds 1 to %d issues, not %d", maxIssuePage, limit)
	}
	return limit, nil
}

var opIssuePositions = newQuery("IssuePositions", "page", `query IssuePositions($filter: IssueFilter, $sort: [IssueSortInput!], $includeArchived: Boolean, $first: Int!, $after: String) {
  page: issues(filter: $filter, sort: $sort, includeArchived: $includeArchived, first: $first, after: $after) {
    nodes { ...PositionFields } pageInfo { hasNextPage endCursor }
  }
}
`+positionFields)

// ListIssuePositions is ListIssues for a caller that needs only each
// issue's ID and manual order: about 1.2 points an issue against
// ListIssues's 10, and no descriptions in the answer.
func (c *Client) ListIssuePositions(ctx context.Context, q IssueQuery, after string) (Page[IssuePosition], error) {
	vars, err := issueVars(q, after)
	if err != nil {
		return Page[IssuePosition]{}, invalid(opIssuePositions.name, "%w", err)
	}
	return fetchPage(ctx, c, opIssuePositions, vars, wirePosition.typed)
}

var opIssueSummary = newQuery("IssueSummary", "issue", `query IssueSummary($id: String!) {
  issue(id: $id) { ...IssueFields }
}
`+issueFields)

// IssueSummary returns one issue by its ID or its identifier, archived or
// not, without what Issue adds: about 10 points against Issue's 286. A
// missing or invisible issue matches trackerapi.ErrNotFound.
func (c *Client) IssueSummary(ctx context.Context, idOrIdentifier string) (*Issue, error) {
	if idOrIdentifier == "" {
		return nil, invalid(opIssueSummary.name, "an issue ID or identifier is required")
	}
	var data struct {
		Issue *wireIssue `json:"issue"`
	}
	if _, err := c.call(ctx, opIssueSummary, idVars{idOrIdentifier}, &data); err != nil {
		return nil, err
	}
	if data.Issue == nil {
		return nil, fmt.Errorf("linear %s: the answer holds no issue", opIssueSummary.name)
	}
	issue := data.Issue.typed()
	return &issue, nil
}

// SearchQuery is a full-text search of issues' titles and descriptions.
type SearchQuery struct {
	// Term is the text to find, passed to Linear as a value; required.
	Term string
	// TeamID limits the search to one team; empty searches every team.
	TeamID          string
	IncludeArchived bool
	// Limit is the page size, 1 to 250; zero means 100.
	Limit int
}

var opSearchIssues = newQuery("SearchIssues", "page", `query SearchIssues($term: String!, $teamId: String, $includeArchived: Boolean, $first: Int!, $after: String) {
  page: searchIssues(term: $term, teamId: $teamId, includeArchived: $includeArchived, first: $first, after: $after) {
    nodes { ...IssueSearchFields } pageInfo { hasNextPage endCursor }
  }
}
`+issueSearchFields).withMaxBody(issueBodyBytes)

type searchVars struct {
	pageVars
	Term            string `json:"term"`
	TeamID          string `json:"teamId,omitempty"`
	IncludeArchived bool   `json:"includeArchived,omitempty"`
}

// SearchIssues returns the page of issues q's term matches that follows
// after, in Linear's order of relevance; Linear's text-search syntax, if
// any, is Linear's to read. Pass the same q for every page of one search.
func (c *Client) SearchIssues(ctx context.Context, q SearchQuery, after string) (Page[Issue], error) {
	if strings.TrimSpace(q.Term) == "" {
		return Page[Issue]{}, invalid(opSearchIssues.name, "a search term is required")
	}
	first, err := pageSize(q.Limit)
	if err != nil {
		return Page[Issue]{}, invalid(opSearchIssues.name, "%w", err)
	}
	vars := searchVars{pageVars{First: first, After: after}, q.Term, q.TeamID, q.IncludeArchived}
	return fetchPage(ctx, c, opSearchIssues, vars, wireIssue.typed)
}

var opIssue = newQuery("Issue", "issue", `query Issue($id: String!) {
  issue(id: $id) {
    ...IssueFields
    project { id name }
    parent { ...IssueRefFields }
    children(first: 50) { nodes { ...IssueRefFields } pageInfo { hasNextPage } }
    comments(first: 50) { nodes { ...CommentFields } pageInfo { hasNextPage endCursor } }
    attachments(first: 25) { nodes { ...AttachmentFields } pageInfo { hasNextPage } }
    relations(first: 25) { nodes { ...RelationFields } pageInfo { hasNextPage endCursor } }
    inverseRelations(first: 25) { nodes { ...RelationFields } pageInfo { hasNextPage endCursor } }
  }
}
`+issueFields+issueRefFields+commentFields+attachmentFields+relationFields)

// namedRef is a related entity selected as { id name }.
type namedRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type wireIssueDetail struct {
	wireIssue
	// Project shadows the fragment's project { id } with its name.
	Project          *namedRef                  `json:"project"`
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
// archived or not, with its relations read to their end: one request,
// and one more for each further page of relations either way. A missing
// or invisible issue matches trackerapi.ErrNotFound.
func (c *Client) Issue(ctx context.Context, idOrIdentifier string) (*IssueDetail, error) {
	if idOrIdentifier == "" {
		return nil, invalid("Issue", "an issue ID or identifier is required")
	}
	var data struct {
		Issue *wireIssueDetail `json:"issue"`
	}
	r, err := c.call(ctx, opIssue, idVars{idOrIdentifier}, &data)
	if err != nil {
		return nil, err
	}
	if data.Issue == nil {
		return nil, fmt.Errorf("linear %s: the answer holds no issue", opIssue.name)
	}
	d, err := data.Issue.detail(r.Usage)
	if err != nil {
		return nil, err
	}
	for _, rel := range []struct {
		op   operation
		conn connection[wireRelation]
	}{{opIssueRelations, data.Issue.Relations}, {opIssueInverseRelations, data.Issue.InverseRelations}} {
		rels, err := c.relations(ctx, rel.op, d.ID, rel.conn)
		if err != nil {
			return nil, err
		}
		d.Relations = append(d.Relations, rels...)
	}
	return d, nil
}

// detail types the answer of the single-issue read, its relations aside.
func (w *wireIssueDetail) detail(usage Usage) (*IssueDetail, error) {
	d := &IssueDetail{Issue: w.typed(), Parent: w.Parent.typedPtr()}
	// The detail's parent and project selections shadow the fragment's
	// { id } ones.
	if d.Parent != nil {
		d.ParentID = d.Parent.ID
	}
	if w.Project != nil {
		d.ProjectID, d.ProjectName = w.Project.ID, w.Project.Name
	}
	var err error
	if d.Children, d.MoreChildren, err = firstPage(opIssue, w.Children, wireIssueRef.typed); err != nil {
		return nil, err
	}
	if d.Comments, err = page(opIssue, w.Comments, usage, wireComment.typed); err != nil {
		return nil, err
	}
	if d.Attachments, d.MoreAttachments, err = firstPage(opIssue, w.Attachments, wireAttachment.typed); err != nil {
		return nil, err
	}
	return d, nil
}

var opIssueRelations = newQuery("IssueRelations", "node", `query IssueRelations($id: String!, $first: Int!, $after: String) {
  node: issue(id: $id) {
    page: relations(first: $first, after: $after) { nodes { ...RelationFields } pageInfo { hasNextPage endCursor } }
  }
}
`+relationFields+issueRefFields)

var opIssueInverseRelations = newQuery("IssueInverseRelations", "node", `query IssueInverseRelations($id: String!, $first: Int!, $after: String) {
  node: issue(id: $id) {
    page: inverseRelations(first: $first, after: $after) { nodes { ...RelationFields } pageInfo { hasNextPage endCursor } }
  }
}
`+relationFields+issueRefFields)

// relations are the relations of first, the page the single-issue read
// held, and of every page after it, which op reads for the issue id.
func (c *Client) relations(ctx context.Context, op operation, id string, first connection[wireRelation]) ([]Relation, error) {
	p, err := page(opIssue, first, Usage{}, wireRelation.typed)
	if err != nil {
		return nil, err
	}
	out := p.Items
	for p.HasNextPage {
		after := p.EndCursor
		if p, err = fetchPage(ctx, c, op, issuePageVars{pageVars{relationPage, after}, id}, wireRelation.typed); err != nil {
			return nil, fmt.Errorf("%w (%d relations were read before it)", err, len(out))
		}
		out = append(out, p.Items...)
		if p.HasNextPage && p.EndCursor == after {
			return nil, fmt.Errorf("linear %s: the page cursor did not advance", op.name)
		}
	}
	return out, nil
}

type issuePageVars struct {
	pageVars
	ID string `json:"id"`
}

var opIssueComments = newQuery("IssueComments", "node", `query IssueComments($id: String!, $first: Int!, $after: String) {
  node: issue(id: $id) {
    page: comments(first: $first, after: $after) { nodes { ...CommentFields } pageInfo { hasNextPage endCursor } }
  }
}
`+commentFields)

// IssueComments returns the page of an issue's comments after the cursor
// after, newest first (Linear's default order); replies carry their
// ParentID. Issue holds the first page and its EndCursor.
func (c *Client) IssueComments(ctx context.Context, issueID, after string) (Page[Comment], error) {
	if issueID == "" {
		return Page[Comment]{}, invalid("IssueComments", "an issue ID is required")
	}
	return fetchPage(ctx, c, opIssueComments, issuePageVars{pageVars{commentPage, after}, issueID}, wireComment.typed)
}

var opIssueHistory = newQuery("IssueHistory", "node", `query IssueHistory($id: String!, $first: Int!, $after: String) {
  node: issue(id: $id) {
    page: history(first: $first, after: $after) { nodes { ...HistoryFields } pageInfo { hasNextPage endCursor } }
  }
}
`+historyFields)

// IssueHistory returns the page of an issue's recorded changes after the
// cursor after, newest first (Linear's default order).
// Changes made in a window after creation that ends between 2m30s and 3m
// (measured by TestLiveHistoryFoldBisect) are folded into the creation,
// not entries, and an entry can be recorded a few seconds after its
// change.
func (c *Client) IssueHistory(ctx context.Context, issueID, after string) (Page[HistoryEntry], error) {
	if issueID == "" {
		return Page[HistoryEntry]{}, invalid("IssueHistory", "an issue ID is required")
	}
	return fetchPage(ctx, c, opIssueHistory, issuePageVars{pageVars{historyPage, after}, issueID}, wireHistory.typed)
}

var opIssueStateSpans = newQuery("IssueStateSpans", "node", `query IssueStateSpans($id: String!, $first: Int!, $after: String) {
  node: issue(id: $id) {
    page: stateHistory(first: $first, after: $after) { nodes { ...StateSpanFields } pageInfo { hasNextPage endCursor } }
  }
}
`+stateSpanFields)

// IssueStateSpans returns the page of periods an issue spent in each
// status after the cursor after, oldest first. Re-entering a status
// starts a new span.
func (c *Client) IssueStateSpans(ctx context.Context, issueID, after string) (Page[StateSpan], error) {
	if issueID == "" {
		return Page[StateSpan]{}, invalid("IssueStateSpans", "an issue ID is required")
	}
	return fetchPage(ctx, c, opIssueStateSpans, issuePageVars{pageVars{historyPage, after}, issueID}, wireStateSpan.typed)
}

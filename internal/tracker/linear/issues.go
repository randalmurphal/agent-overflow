package linear

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Page sizes of the issue reads. Linear charges a page of issues by the
// issues it returns (8 points for 42 issues with their labels at first:
// 100 and at first: 250, measured), not by the page size, so a list
// walked to its end pages at maxIssuePage. Lists carry no descriptions;
// a page's body bound stays issueBodyBytes for an issue whose 250 nested
// labels and long title run large. A history page is charged by its
// first (203 points at 50, 403 at 100, 1,003 at 250, measured), so its
// cost per entry falls with the page and it pages at the largest. The
// single-issue read costs about 286 points with its comments,
// attachments and relations (measured), plus its history page and its
// team's states. It nests detailHistoryPage entries (403 points), which
// holds the whole history of most issues; a longer one continues at
// historyPage. A comment page and a relation page are charged by first
// by the documented rule. The single-issue read nests 50 of its team's
// states: a catalog connection, charged by first, where a team rarely
// has more than a dozen.
const (
	issuePage         = maxIssuePage
	issueBodyBytes    = 32 << 20
	commentPage       = 100
	historyPage       = maxIssuePage
	detailHistoryPage = 100
	relationPage      = 100
)

// IssueOrder orders a list of issues.
type IssueOrder int

const (
	// OrderManual is the manual order of board columns and the backlog:
	// SortOrder, lowest first. A rank changes it, so a walk of more than
	// one page in this order can skip or repeat issues: a page cursor
	// follows its issue (measured).
	OrderManual IssueOrder = iota
	// OrderPriority is by priority, urgent first, then PrioritySortOrder
	// within a priority.
	OrderPriority
	OrderCreatedNewest
	// OrderCreatedOldest is by creation time, oldest first: an order no
	// change moves an issue in, so a walk by cursor neither skips nor
	// repeats one, and an issue created during the walk comes last.
	OrderCreatedOldest
	// orderUpdatedOldest and orderUpdatedNewest are the orders of a
	// walk by position (poll.go): an update moves an issue in them, so
	// no page in them follows a Linear cursor, and only the walks by
	// position use them.
	orderUpdatedOldest
	orderUpdatedNewest
)

// maxIssuePage is the largest page Linear serves, outer or nested:
// first: 251 answers INVALID_INPUT (measured).
const maxIssuePage = 250

// IssueQuery selects and orders issues.
type IssueQuery struct {
	Filter          IssueFilter
	Order           IssueOrder
	IncludeArchived bool
	// Limit is the page size, 1 to 250; zero means 250. Linear charges a
	// page by what it returns, so a larger page costs no more.
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
	case OrderCreatedNewest:
		return []issueSort{{CreatedAt: desc}}, nil
	case OrderCreatedOldest:
		return []issueSort{{CreatedAt: asc}}, nil
	case orderUpdatedOldest:
		return []issueSort{{UpdatedAt: asc}}, nil
	case orderUpdatedNewest:
		return []issueSort{{UpdatedAt: desc}}, nil
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
	if after != "" && (q.Order == orderUpdatedOldest || q.Order == orderUpdatedNewest) {
		return issuesVars{}, fmt.Errorf("a walk in update order pages by position, not by a page cursor")
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
// issue's ID, manual order and team: about 4 points a page against
// ListIssues's 8 (measured for 48 issues with id and updatedAt, and for
// 42 with card fields), and about 90 bytes an issue against 1 KB.
func (c *Client) ListIssuePositions(ctx context.Context, q IssueQuery, after string) (Page[IssuePosition], error) {
	vars, err := issueVars(q, after)
	if err != nil {
		return Page[IssuePosition]{}, invalid(opIssuePositions.name, "%w", err)
	}
	return fetchPage(ctx, c, opIssuePositions, vars, wirePosition.typed)
}

var opIssueSummary = newQuery("IssueSummary", "issue", `query IssueSummary($id: String!) {
  issue(id: $id) { ...DescribedFields }
}
`+describedFields+issueFields)

// IssueSummary returns one issue by its ID or its identifier, archived or
// not, with its description and without what Issue adds: one object
// with its labels, a few points against Issue's thousand and more. A
// missing or invisible issue matches
// trackerapi.ErrNotFound.
func (c *Client) IssueSummary(ctx context.Context, idOrIdentifier string) (*DescribedIssue, error) {
	if idOrIdentifier == "" {
		return nil, invalid(opIssueSummary.name, "an issue ID or identifier is required")
	}
	var data struct {
		Issue *wireDescribed `json:"issue"`
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
	// Limit is the page size, 1 to 250; zero means 250.
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

// opIssue reads one issue with everything its view shows. Its team's
// states are aliased stateTeam beside the fragment's team so the two
// selections decode apart.
var opIssue = newQuery("Issue", "issue", `query Issue($id: String!) {
  issue(id: $id) {
    ...DescribedFields
    project { id name }
    parent { ...IssueRefFields }
    children(first: 50) { nodes { ...IssueRefFields } pageInfo { hasNextPage } }
    comments(first: 50) { nodes { ...CommentFields } pageInfo { hasNextPage endCursor } }
    attachments(first: 25) { nodes { ...AttachmentFields } pageInfo { hasNextPage } }
    relations(first: 25) { nodes { ...RelationFields } pageInfo { hasNextPage endCursor } }
    inverseRelations(first: 25) { nodes { ...RelationFields } pageInfo { hasNextPage endCursor } }
    history(first: `+strconv.Itoa(detailHistoryPage)+`) { nodes { ...HistoryFields } pageInfo { hasNextPage endCursor } }
    stateTeam: team { states(first: 50) { nodes { ...StateFields } pageInfo { hasNextPage } } }
  }
}
`+describedFields+issueFields+issueRefFields+commentFields+attachmentFields+relationFields+historyFields+stateFields).withMaxBody(issueBodyBytes)

// namedRef is a related entity selected as { id name }.
type namedRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// stateTeam is an issue's team as a document selects its states.
type stateTeam struct {
	States connection[wireState] `json:"states"`
}

type wireIssueDetail struct {
	wireDescribed
	// Project shadows the fragment's project { id } with its name.
	Project          *namedRef                  `json:"project"`
	Parent           *wireIssueRef              `json:"parent"`
	Children         connection[wireIssueRef]   `json:"children"`
	Comments         connection[wireComment]    `json:"comments"`
	Attachments      connection[wireAttachment] `json:"attachments"`
	Relations        connection[wireRelation]   `json:"relations"`
	InverseRelations connection[wireRelation]   `json:"inverseRelations"`
	History          connection[wireHistory]    `json:"history"`
	StateTeam        *stateTeam                 `json:"stateTeam"`
}

type idVars struct {
	ID string `json:"id"`
}

// Issue returns one issue by its ID or its identifier, such as WEB-1,
// archived or not, with its description, the first pages of what its
// view shows, its team's states and its relations read to their end:
// one request, and one more for each further page of relations either
// way. A missing or invisible issue matches trackerapi.ErrNotFound.
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
	d, err := data.Issue.detail(r)
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
func (w *wireIssueDetail) detail(r reply) (*IssueDetail, error) {
	d := &IssueDetail{DescribedIssue: w.typed(), Parent: w.Parent.typedPtr()}
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
	if d.Comments, err = page(opIssue, w.Comments, r.Usage, wireComment.typed); err != nil {
		return nil, err
	}
	if d.Attachments, d.MoreAttachments, err = firstPage(opIssue, w.Attachments, wireAttachment.typed); err != nil {
		return nil, err
	}
	if d.History, err = page(opIssue, w.History, r.Usage, wireHistory.typed); err != nil {
		return nil, err
	}
	d.Comments.Served, d.History.Served = r.Served, r.Served
	if w.StateTeam == nil {
		return nil, fmt.Errorf("linear %s: the answer holds no team", opIssue.name)
	}
	if d.States, d.MoreStates, err = firstPage(opIssue, w.StateTeam.States, wireState.typed); err != nil {
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
// Relations page in Linear's default order, by creation, which no
// change moves a relation in.
func (c *Client) relations(ctx context.Context, op operation, id string, first connection[wireRelation]) ([]Relation, error) {
	p, err := page(opIssue, first, Usage{}, wireRelation.typed)
	if err != nil {
		return nil, err
	}
	out := p.Items
	for p.HasNextPage {
		if p, err = fetchPage(ctx, c, op, issuePageVars{pageVars{relationPage, p.EndCursor}, id}, wireRelation.typed); err != nil {
			return nil, fmt.Errorf("%w (%d relations were read before it)", err, len(out))
		}
		out = append(out, p.Items...)
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
// after, newest first (Linear's default order, by creation, which an
// edit does not move a comment in); replies carry their ParentID. Issue
// holds the first page and its EndCursor.
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
`+historyFields).withMaxBody(issueBodyBytes)

// IssueHistory returns the page of an issue's recorded changes after the
// cursor after, newest first (Linear's default order, by creation: an
// entry recorded during a walk comes before its cursor and is left to
// the next read).
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

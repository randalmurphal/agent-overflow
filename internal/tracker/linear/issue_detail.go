package linear

import (
	"context"
	"fmt"

	"agent-overflow/internal/tracker/trackerapi"
)

// opIssue reads one issue with everything its view shows. Its team's
// states are aliased stateTeam beside the fragment's team so the two
// selections decode apart.
var opIssue = newQuery("Issue", "issue", `query Issue($id: String!) {
  issue(id: $id) {`+issueDetailSelection+`  }
}
`+issueDetailFragments).withMaxBody(issueBodyBytes)

// issueDetailSelection is what the ticket view reads of an issue, the
// selection of both opIssue and opIssueDetails, with the fragments it
// spreads in issueDetailFragments.
const issueDetailSelection = `
    ...DescribedFields
    project { id name }
    parent { ...IssueRefFields }
    children(first: 50) { nodes { ...IssueRefFields } pageInfo { hasNextPage } }
    comments(first: 50) { nodes { ...CommentFields } pageInfo { hasNextPage endCursor } }
    attachments(first: 25) { nodes { ...AttachmentFields } pageInfo { hasNextPage } }
    relations(first: 25) { nodes { ...RelationFields } pageInfo { hasNextPage endCursor } }
    inverseRelations(first: 25) { nodes { ...RelationFields } pageInfo { hasNextPage endCursor } }
    history(first: 100) { nodes { ...HistoryFields } pageInfo { hasNextPage endCursor } }
    stateTeam: team { states(first: 50) { nodes { ...StateFields } pageInfo { hasNextPage } } }
`

const issueDetailFragments = describedFields + issueFields + issueRefFields + commentFields + attachmentFields + relationFields + historyFields + stateFields

// MaxIssueDetails is the most issues one IssueDetails request reads:
// Linear charged the same points for 1, 2, 5 and 10 issues in full
// (measured, docs/references/tracker-api-measurements.md), and no more
// were measured.
const MaxIssueDetails = 10

// opIssueDetails reads several issues with everything their view shows,
// by ID, archived or not, in one request.
var opIssueDetails = newQuery("IssueDetails", "page", `query IssueDetails($filter: IssueFilter, $first: Int!) {
  page: issues(filter: $filter, includeArchived: true, first: $first) {
    nodes {`+issueDetailSelection+`    }
    pageInfo { hasNextPage }
  }
}
`+issueDetailFragments).withMaxBody(issueBodyBytes)

type issueDetailsVars struct {
	Filter *issueFilterInput `json:"filter"`
	First  int               `json:"first"`
}

// IssueDetails returns the issues ids name (lowercase UUIDs, at most
// MaxIssueDetails, each once) as Issue does, in one request and one more
// for each further page of relations of any of them, as one
// trackerapi.Walk; an ID that names no issue the user can see is left
// out, with no error. The order is Linear's.
func (c *Client) IssueDetails(ctx context.Context, ids []string) ([]*IssueDetail, error) {
	if len(ids) == 0 || len(ids) > MaxIssueDetails {
		return nil, invalid(opIssueDetails.name, "1 to %d issue IDs are required, not %d", MaxIssueDetails, len(ids))
	}
	filter, err := IssueFilter{IDs: ids}.input()
	if err != nil {
		return nil, invalid(opIssueDetails.name, "%w", err)
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]*IssueDetail, error) { return c.issueDetails(ctx, filter, len(ids)) })
}

// issueDetails is one walk of IssueDetails, of the n issues filter names.
func (c *Client) issueDetails(ctx context.Context, filter *issueFilterInput, n int) ([]*IssueDetail, error) {
	var data struct {
		Page *connection[wireIssueDetail] `json:"page"`
	}
	r, err := c.call(ctx, opIssueDetails, issueDetailsVars{Filter: filter, First: n}, &data)
	if err != nil {
		return nil, err
	}
	if data.Page == nil {
		return nil, fmt.Errorf("linear %s: the answer holds no issues", opIssueDetails.name)
	}
	if more, err := data.Page.more(opIssueDetails); err != nil || more {
		return nil, fmt.Errorf("linear %s: %d IDs answered with more than one page: %v", opIssueDetails.name, n, err)
	}
	out := make([]*IssueDetail, 0, len(data.Page.Nodes))
	for i := range data.Page.Nodes {
		d, err := c.related(ctx, opIssueDetails, &data.Page.Nodes[i], r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

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
// way, as one trackerapi.Walk. A missing or invisible issue matches
// trackerapi.ErrNotFound.
func (c *Client) Issue(ctx context.Context, idOrIdentifier string) (*IssueDetail, error) {
	if idOrIdentifier == "" {
		return nil, invalid("Issue", "an issue ID or identifier is required")
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (*IssueDetail, error) { return c.issue(ctx, idOrIdentifier) })
}

// issue is one walk of Issue.
func (c *Client) issue(ctx context.Context, idOrIdentifier string) (*IssueDetail, error) {
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
	return c.related(ctx, opIssue, data.Issue, r)
}

// related is w, an issue op read, typed, with its relations read to their
// end.
func (c *Client) related(ctx context.Context, op operation, w *wireIssueDetail, r reply) (*IssueDetail, error) {
	d, err := w.detail(op, r)
	if err != nil {
		return nil, err
	}
	for _, rel := range []struct {
		op   operation
		conn connection[wireRelation]
	}{{opIssueRelations, w.Relations}, {opIssueInverseRelations, w.InverseRelations}} {
		rels, err := c.relations(ctx, rel.op, d.ID, rel.conn)
		if err != nil {
			return nil, err
		}
		d.Relations = append(d.Relations, rels...)
	}
	return d, nil
}

// detail types the answer op read of one issue, its relations aside.
func (w *wireIssueDetail) detail(op operation, r reply) (*IssueDetail, error) {
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
	if d.Children, d.MoreChildren, err = firstPage(op, w.Children, wireIssueRef.typed); err != nil {
		return nil, err
	}
	if d.Comments, err = page(op, w.Comments, r.Usage, wireComment.typed); err != nil {
		return nil, err
	}
	if d.Attachments, d.MoreAttachments, err = firstPage(op, w.Attachments, wireAttachment.typed); err != nil {
		return nil, err
	}
	if d.History, err = page(op, w.History, r.Usage, wireHistory.typed); err != nil {
		return nil, err
	}
	d.Comments.Served, d.History.Served = r.Served, r.Served
	if w.StateTeam == nil {
		return nil, fmt.Errorf("linear %s: the answer holds no team", op.name)
	}
	if d.States, d.MoreStates, err = firstPage(op, w.StateTeam.States, wireState.typed); err != nil {
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

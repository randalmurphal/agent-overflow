package jira

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"agent-overflow/internal/tracker/trackerapi"
)

// IssueList names one of Jira Software's issue lists: a board's issues,
// its backlog, a sprint's issues (on one board or all of them), or an
// epic's issues. The zero IssueList is refused.
type IssueList struct {
	kind   listKind
	board  int64
	sprint int64
	epic   string
}

type listKind uint8

const (
	listNone listKind = iota
	listBoard
	listBacklog
	listSprint
	listBoardSprint
	listEpic
)

// BoardIssues are the issues in a board's columns, in rank order. A scrum
// board lists no epics.
func BoardIssues(board int64) IssueList { return IssueList{kind: listBoard, board: board} }

// BacklogIssues are a board's unfinished issues in no future or active
// sprint, in rank order.
func BacklogIssues(board int64) IssueList { return IssueList{kind: listBacklog, board: board} }

// SprintIssues are every issue of a sprint the user can see, in rank
// order.
func SprintIssues(sprint int64) IssueList { return IssueList{kind: listSprint, sprint: sprint} }

// BoardSprintIssues are the issues of a sprint that are on a board.
func BoardSprintIssues(board, sprint int64) IssueList {
	return IssueList{kind: listBoardSprint, board: board, sprint: sprint}
}

// EpicIssues are the issues of a company-managed project's epic, named
// by key or ID. A team-managed project's epic is a parent: search with
// the parent clause instead.
func EpicIssues(epic string) IssueList { return IssueList{kind: listEpic, epic: epic} }

// path is the list's path from the site root, its references checked.
func (l IssueList) path() (string, error) {
	board, sprint := strconv.FormatInt(l.board, 10), strconv.FormatInt(l.sprint, 10)
	var path string
	var err error
	switch l.kind {
	case listNone:
		return "", notSent("no issue list named")
	case listBoard:
		path, err = softwarePath+"board/"+board+"/issue", checkAgileID("board", l.board)
	case listBacklog:
		path, err = softwarePath+"board/"+board+"/backlog", checkAgileID("board", l.board)
	case listSprint:
		path, err = softwarePath+"sprint/"+sprint+"/issue", checkAgileID("sprint", l.sprint)
	case listBoardSprint:
		path, err = softwarePath+"board/"+board+"/sprint/"+sprint+"/issue", checkAgileID("board", l.board)
		if err == nil {
			err = checkAgileID("sprint", l.sprint)
		}
	case listEpic:
		path, err = softwarePath+"epic/"+url.PathEscape(l.epic)+"/issue", checkIssue(l.epic)
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

// ListQuery is one page of an issue list.
type ListQuery struct {
	List IssueList
	// JQL narrows the list; an ORDER BY in it replaces rank order.
	JQL string
	// Fields are the field IDs each issue carries; none returns Jira's
	// navigable and Jira Software fields.
	Fields []string
	// MaxResults bounds the page; zero means 100. Jira may return fewer.
	MaxResults int
	// PageToken continues the list from the page that named it.
	PageToken string
}

// ListIssues reads one page of an issue list. Its warnings are Jira's
// notes on the JQL, with no Type. A 400 to a page narrowed by JQL is a
// *QueryRefusedError.
func (c *Client) ListIssues(ctx context.Context, q ListQuery) (SearchPage, error) {
	if err := c.admit(ctx); err != nil {
		return SearchPage{}, err
	}
	return c.listPage(ctx, q)
}

func (c *Client) listPage(ctx context.Context, q ListQuery) (SearchPage, error) {
	path, err := q.List.path()
	if err != nil {
		return SearchPage{}, err
	}
	if q.MaxResults < 0 {
		return SearchPage{}, notSent("an issue list page of %d issues", q.MaxResults)
	}
	v := url.Values{"maxResults": {strconv.Itoa(pageSize(q.MaxResults, 100, maxSearchPage))}}
	if q.JQL != "" {
		v.Set("jql", q.JQL)
	}
	if q.PageToken != "" {
		v.Set("nextPageToken", q.PageToken)
	}
	// The spec types fields as a list; a repeated parameter is a list
	// however the server splits it.
	for _, f := range q.Fields {
		v.Add("fields", f)
	}
	var j struct {
		Issues        []issueJSON `json:"issues"`
		NextPageToken string      `json:"nextPageToken"`
		IsLast        bool        `json:"isLast"`
		Warnings      []string    `json:"warningMessages"`
	}
	resp, err := c.call(ctx, trackerapi.Request{Path: path, Query: v}, &j)
	if err != nil {
		if q.JQL != "" {
			err = refusedQuery(err)
		}
		return SearchPage{}, fmt.Errorf("jira: issue list: %w", err)
	}
	page := SearchPage{Issues: make([]*Issue, len(j.Issues)), Date: answerDate(resp)}
	for i, is := range j.Issues {
		page.Issues[i] = is.decode()
	}
	if page.NextPageToken, err = nextToken(j.IsLast, j.NextPageToken); err != nil {
		return SearchPage{}, fmt.Errorf("jira: issue list: %w", err)
	}
	for _, w := range j.Warnings {
		page.Warnings = append(page.Warnings, SearchWarning{Message: w})
	}
	return page, nil
}

// ListAllIssues reads an issue list from its first page (or
// q.PageToken) to the last, at most limit issues when limit is above
// zero, as SearchAll reads a search: Complete is false when the limit cut
// it or Jira warned, and a repeated token or an empty page that names a
// next one fails it.
func (c *Client) ListAllIssues(ctx context.Context, q ListQuery, limit int) (SearchResult, error) {
	if err := c.admit(ctx); err != nil {
		return SearchResult{}, err
	}
	return c.listAll(ctx, q, limit)
}

func (c *Client) listAll(ctx context.Context, q ListQuery, limit int) (SearchResult, error) {
	size := pageSize(q.MaxResults, 100, maxSearchPage)
	first := q.PageToken
	return trackerapi.Walk(ctx, func(ctx context.Context) (SearchResult, error) {
		return tokenPages("issue list", first, limit, func(token string, room int) (SearchPage, error) {
			q.PageToken, q.MaxResults = token, size
			if room > 0 {
				q.MaxResults = min(size, room)
			}
			return c.listPage(ctx, q)
		})
	})
}

// CountListIssues returns Jira's approximate count of a board's issues
// or its backlog, narrowed by jql when it is set. Jira counts no other
// list, so any other is refused before anything is sent.
func (c *Client) CountListIssues(ctx context.Context, l IssueList, jql string) (int64, error) {
	if err := c.admit(ctx); err != nil {
		return 0, err
	}
	if l.kind != listBoard && l.kind != listBacklog {
		return 0, notSent("Jira counts a board's issues and its backlog only")
	}
	path, err := l.path()
	if err != nil {
		return 0, err
	}
	v := url.Values{}
	if jql != "" {
		v.Set("jql", jql)
	}
	var j struct {
		Count int64 `json:"count"`
	}
	if _, err := c.call(ctx, trackerapi.Request{Path: path + "/approximate-count", Query: v}, &j); err != nil {
		return 0, fmt.Errorf("jira: count list issues: %w", err)
	}
	return j.Count, nil
}

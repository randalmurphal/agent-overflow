package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// worklogListLimit is the most worklogs one WorklogsByID request names.
const worklogListLimit = 1000

// WorklogChange is a worklog updated or deleted, and when.
type WorklogChange struct {
	ID int64
	At time.Time
}

// WorklogChanges is what a sync read returned, oldest first. Until is
// the time of the last change read: the since of the next read.
// Complete is false when the walk stopped early.
type WorklogChanges struct {
	Changes  []WorklogChange
	Until    time.Time
	Complete bool
}

// UpdatedWorklogs lists the worklogs the user can see that were updated
// or created after since, every page of them. Jira leaves out the
// changes of the last minute, so a sync loop reads again from Until.
func (c *Client) UpdatedWorklogs(ctx context.Context, since time.Time) (WorklogChanges, error) {
	return c.worklogChanges(ctx, "updated", since)
}

// DeletedWorklogs lists the worklogs deleted after since, as
// UpdatedWorklogs lists updates.
func (c *Client) DeletedWorklogs(ctx context.Context, since time.Time) (WorklogChanges, error) {
	return c.worklogChanges(ctx, "deleted", since)
}

// worklogChanges walks worklog/updated or worklog/deleted. Each page
// that is not the last continues from its until, as Jira's nextPage
// does; a page whose until does not move forward fails the walk rather
// than loop. The pages are one walk (walk.go).
func (c *Client) worklogChanges(ctx context.Context, kind string, since time.Time) (WorklogChanges, error) {
	out := WorklogChanges{Until: since}
	if err := c.admit(ctx); err != nil {
		return out, err
	}
	if since.Before(time.UnixMilli(0)) {
		return out, notSent("%s worklogs since %s, before 1970", kind, since.Format(time.RFC3339))
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (WorklogChanges, error) { return c.worklogChangeWalk(ctx, kind, since) })
}

// worklogChangeWalk is one walk of worklogChanges.
func (c *Client) worklogChangeWalk(ctx context.Context, kind string, since time.Time) (WorklogChanges, error) {
	out := WorklogChanges{Until: since}
	from := since.UnixMilli()
	for {
		var page struct {
			Values []struct {
				ID      int64 `json:"worklogId"`
				Updated int64 `json:"updatedTime"`
			} `json:"values"`
			Until    int64 `json:"until"`
			LastPage bool  `json:"lastPage"`
		}
		q := url.Values{"since": {strconv.FormatInt(from, 10)}}
		if err := c.get(ctx, "worklog/"+kind, q, &page); err != nil {
			return out, fmt.Errorf("jira: %s worklogs: %w", kind, err)
		}
		for _, v := range page.Values {
			out.Changes = append(out.Changes, WorklogChange{ID: v.ID, At: time.UnixMilli(v.Updated)})
		}
		if page.Until > 0 {
			out.Until = time.UnixMilli(page.Until)
		}
		switch {
		case page.LastPage:
			out.Complete = true
			return out, nil
		case page.Until <= from:
			return out, fmt.Errorf("jira: %s worklogs: a page that is not the last ends at %d, not after %d", kind, page.Until, from)
		}
		from = page.Until
	}
}

// WorklogsByID reads the worklogs with the IDs given, 1000 a request,
// for a sync loop that learned them from UpdatedWorklogs. Jira leaves out
// the ones the user cannot see, so Complete is false when fewer came
// back than were named; a worklog Jira returns that was not named is
// left out. A failure returns what was read with the error.
func (c *Client) WorklogsByID(ctx context.Context, ids []int64) (WorklogList, error) {
	var out WorklogList
	if err := c.admit(ctx); err != nil {
		return out, err
	}
	if len(ids) == 0 {
		return out, notSent("no worklog IDs given")
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if err := checkAgileID("worklog", id); err != nil {
			return out, err
		}
		if seen[id] {
			return out, notSent("worklog %d is named twice", id)
		}
		seen[id] = true
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (WorklogList, error) { return c.worklogsByID(ctx, ids) })
}

// worklogsByID is one walk of WorklogsByID.
func (c *Client) worklogsByID(ctx context.Context, ids []int64) (WorklogList, error) {
	var out WorklogList
	named := make(map[int64]bool, len(ids))
	for _, id := range ids {
		named[id] = true
	}
	for group := range slices.Chunk(ids, worklogListLimit) {
		var page []worklogJSON
		_, err := c.call(ctx, trackerapi.Request{
			Method: http.MethodPost, Path: apiPath + "worklog/list", Body: map[string]any{"ids": group}, ReadOnly: true,
		}, &page)
		if err != nil {
			return out, fmt.Errorf("jira: worklogs by ID: %w", err)
		}
		for _, j := range page {
			w := j.worklog()
			id, _ := strconv.ParseInt(w.ID, 10, 64)
			if named[id] {
				named[id] = false // each named worklog counts once
				out.Worklogs = append(out.Worklogs, w)
			}
		}
	}
	out.Complete = len(out.Worklogs) == len(ids)
	return out, nil
}

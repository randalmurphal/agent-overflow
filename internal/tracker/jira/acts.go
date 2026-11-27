package jira

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Act is a comment or a worklog as a count of who worked on an issue reads
// it: its ID, its author, when it was made (a comment's creation, a
// worklog's start) and a worklog's time spent. Its text is not read: the
// decoders name no body or comment member, so the answer's text is
// skipped unread rather than decoded and kept.
type Act struct {
	ID string
	// Author is nil when Jira named no account (an author since deleted).
	Author *User
	At     time.Time
	Spent  time.Duration
}

// ActList is the acts of one kind a read returned, oldest first, and
// whether they are every one the issue has (an embedded list may hold
// the first ones only).
type ActList struct {
	Acts     []Act
	Complete bool
}

// IssueActs is an issue's acts as a bulk fetch embedded them, with the
// issue's update time.
type IssueActs struct {
	ID       string
	Updated  time.Time
	Comments ActList
	// Worklogs is zero when the read did not ask for them.
	Worklogs ActList
}

type commentActJSON struct {
	ID      string    `json:"id"`
	Author  *userJSON `json:"author"`
	Created string    `json:"created"`
}

type worklogActJSON struct {
	ID               string    `json:"id"`
	Author           *userJSON `json:"author"`
	Started          string    `json:"started"`
	TimeSpentSeconds int64     `json:"timeSpentSeconds"`
}

func (j commentActJSON) act() (Act, error) { return actOf(j.ID, j.Author, "created", j.Created, 0) }

func (j worklogActJSON) act() (Act, error) {
	return actOf(j.ID, j.Author, "started", j.Started, j.TimeSpentSeconds)
}

// actOf is an act of id by author made at text (the member name names),
// spending seconds; one with no ID or time, or a time that does not
// parse, is a malformed answer: it cannot be counted, and counting the
// issue without it would count less than happened.
func actOf(id string, author *userJSON, name, text string, seconds int64) (Act, error) {
	at, err := parseTime(text)
	if id == "" || err != nil || seconds < 0 {
		return Act{}, &AnswerError{Message: fmt.Sprintf("malformed answer: act %q has %s %q and %d seconds", id, name, text, seconds)}
	}
	return Act{ID: id, Author: author.userPtr(), At: at, Spent: time.Duration(seconds) * time.Second}, nil
}

// actsIssueJSON is an issue of a bulk fetch of its acts.
type actsIssueJSON struct {
	ID     string `json:"id"`
	Fields struct {
		Updated string                           `json:"updated"`
		Comment *commentPageJSON[commentActJSON] `json:"comment"`
		Worklog *struct {
			StartAt  int              `json:"startAt"`
			Total    *int             `json:"total"`
			Worklogs []worklogActJSON `json:"worklogs"`
		} `json:"worklog"`
	} `json:"fields"`
}

func (v actsIssueJSON) issueID() string { return v.ID }

// actList is the acts of an embedded list of total, from startAt: complete
// when it starts at the first and holds the total. A list with no total
// is a malformed answer.
func actList[J interface{ act() (Act, error) }](js []J, startAt int, total *int) (ActList, error) {
	if total == nil {
		return ActList{}, &AnswerError{Message: "malformed answer: an embedded list names no total"}
	}
	out := ActList{Acts: make([]Act, 0, len(js)), Complete: startAt == 0 && len(js) >= *total}
	for _, j := range js {
		a, err := j.act()
		if err != nil {
			return ActList{}, err
		}
		out.Acts = append(out.Acts, a)
	}
	return out, nil
}

// BulkActs reads, by issue ID, each issue's update time and the comments,
// and with worklogs the worklogs, Jira embeds in it, in one bulk fetch per
// batch (BulkFetchByID's walk). An issue Jira does not return (deleted,
// hidden, or one it could not read) is left out. An embedded list may hold
// the first comments or worklogs only (ActList.Complete); CommentActs and
// WorklogActs read the rest.
func (c *Client) BulkActs(ctx context.Context, ids []string, worklogs bool) ([]IssueActs, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := checkID("issue", id); err != nil {
			return nil, err
		}
	}
	fields := []string{"comment", "updated"}
	if worklogs {
		fields = append(fields, "worklog")
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]IssueActs, error) {
		var out []IssueActs
		var decodeErr error
		_, _, left, err := bulkFetchEach(ctx, c, ids, fields, true, func(issues []actsIssueJSON, _ *trackerapi.Response, _ uint64) {
			for _, is := range issues {
				if decodeErr != nil {
					return
				}
				var acts IssueActs
				acts, decodeErr = is.acts(worklogs)
				out = append(out, acts)
			}
		})
		if err == nil {
			err = decodeErr
		}
		if err == nil {
			err = c.prove(ctx, "a bulk fetch of comments and worklogs", left)
		}
		if err != nil {
			return nil, fmt.Errorf("jira: comments and worklogs of %d issues: %w", len(ids), err)
		}
		return out, nil
	})
}

// acts is v's acts; an issue with no update time, or whose comment field,
// or worklog field when worklogs, is missing, is a malformed answer.
func (v actsIssueJSON) acts(worklogs bool) (IssueActs, error) {
	f := v.Fields
	updated, err := parseTime(f.Updated)
	if err != nil || f.Comment == nil || worklogs && f.Worklog == nil {
		return IssueActs{}, &AnswerError{Message: fmt.Sprintf("malformed answer: issue %s came without its update time or acts", v.ID)}
	}
	out := IssueActs{ID: v.ID, Updated: updated}
	if out.Comments, err = actList(f.Comment.Comments, f.Comment.StartAt, f.Comment.Total); err != nil {
		return IssueActs{}, fmt.Errorf("issue %s comments: %w", v.ID, err)
	}
	if worklogs {
		if out.Worklogs, err = actList(f.Worklog.Worklogs, f.Worklog.StartAt, f.Worklog.Total); err != nil {
			return IssueActs{}, fmt.Errorf("issue %s worklogs: %w", v.ID, err)
		}
	}
	return out, nil
}

// CommentActs reads every comment of issue as an act, oldest first, page
// by page (commentPages).
func (c *Client) CommentActs(ctx context.Context, issue string) ([]Act, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]Act, error) {
		var out []Act
		err := commentPages(ctx, c, issue, func(j commentActJSON) error {
			a, err := j.act()
			out = append(out, a)
			return err
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	})
}

// WorklogActs reads the worklogs of q's issue that q picks as acts, oldest
// first, page by page (worklogPages).
func (c *Client) WorklogActs(ctx context.Context, q WorklogQuery) ([]Act, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]Act, error) {
		var out []Act
		err := worklogPages(ctx, c, q, func(j worklogActJSON) error {
			a, err := j.act()
			out = append(out, a)
			return err
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	})
}

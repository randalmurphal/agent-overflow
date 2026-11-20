package tempo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Issue names a Jira issue. Tempo stores the numeric ID. Key is the key
// a write named the issue by, and is empty on a worklog read from Tempo,
// which names the issue by ID only.
type Issue struct {
	ID  int64
	Key string
}

// AttributeValue is one work attribute's value on a worklog: the
// attribute's key and the value Tempo stores (for a static list, the
// option's immutable value, not its display name; for an account, the
// account key; for a checkbox, "true" or "false").
type AttributeValue struct {
	Key   string
	Value string
}

// Worklog is a Tempo worklog as read.
type Worklog struct {
	// ID is Tempo's worklog ID. Tempo v4 no longer returns the ID of the
	// Jira worklog it syncs to.
	ID     int64
	Issue  Issue
	Author string // Atlassian account ID
	Start  Start
	// StartUTC is Tempo's conversion of Start to UTC, made with the
	// author's browser or profile time zone when the worklog was created.
	// It is informative only and zero when Tempo did not give it.
	StartUTC time.Time
	Spent    time.Duration
	// Billable is the billable part of Spent; nil when Tempo did not
	// report it. A change keeps it, so a record rebuilt from storage for
	// an update carries it as read.
	Billable    *time.Duration
	Description string
	Attributes  []AttributeValue
	Created     time.Time
	Updated     time.Time
}

// worklogJSON is a worklog on the wire.
type worklogJSON struct {
	ID    int64 `json:"tempoWorklogId"`
	Issue struct {
		ID int64 `json:"id"`
	} `json:"issue"`
	Author struct {
		AccountID string `json:"accountId"`
	} `json:"author"`
	StartDate   string `json:"startDate"`
	StartTime   string `json:"startTime"`
	StartUTC    string `json:"startDateTimeUtc"`
	Spent       int64  `json:"timeSpentSeconds"`
	Billable    *int64 `json:"billableSeconds"`
	Description string `json:"description"`
	Attributes  struct {
		Values []attributeValueJSON `json:"values"`
	} `json:"attributes"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type attributeValueJSON struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// worklog reads w, refusing a record whose dates or times do not parse.
// resp is the answer w came in, whose body the transport scrubbed of the
// credential the request carried; the text of an error is scrubbed too.
func (w worklogJSON) worklog(resp *trackerapi.Response) (Worklog, error) {
	out, err := w.read()
	if err != nil {
		return Worklog{}, unreadable(resp, err)
	}
	return out, nil
}

func (w worklogJSON) read() (Worklog, error) {
	out := Worklog{
		ID:          w.ID,
		Issue:       Issue{ID: w.Issue.ID},
		Author:      w.Author.AccountID,
		Spent:       duration(w.Spent),
		Description: w.Description,
	}
	if w.Billable != nil {
		b := duration(*w.Billable)
		out.Billable = &b
	}
	var err error
	if out.Start.Date, err = ParseDate(w.StartDate); err != nil {
		return Worklog{}, fmt.Errorf("worklog %d: start date: %w", w.ID, err)
	}
	if w.StartTime != "" {
		if out.Start.Time, err = ParseTimeOfDay(w.StartTime); err != nil {
			return Worklog{}, fmt.Errorf("worklog %d: start time: %w", w.ID, err)
		}
	}
	for _, ts := range []struct {
		name string
		text string
		dst  *time.Time
	}{{"startDateTimeUtc", w.StartUTC, &out.StartUTC}, {"createdAt", w.CreatedAt, &out.Created}, {"updatedAt", w.UpdatedAt, &out.Updated}} {
		if ts.text == "" {
			continue
		}
		if *ts.dst, err = parseTimestamp(ts.text); err != nil {
			return Worklog{}, fmt.Errorf("worklog %d: %s: %w", w.ID, ts.name, err)
		}
	}
	if len(w.Attributes.Values) > 0 {
		out.Attributes = make([]AttributeValue, len(w.Attributes.Values))
		for i, v := range w.Attributes.Values {
			out.Attributes[i] = AttributeValue(v)
		}
	}
	return out, nil
}

// WorklogQuery selects one author's worklogs by start date.
type WorklogQuery struct {
	// Author is the Atlassian account ID. Required.
	Author string
	// From and To are the first and last start dates, both included.
	// Required.
	From, To Date
	// UpdatedFrom, when set, keeps only worklogs created or changed at or
	// after it. Tempo cannot report deletions this way.
	UpdatedFrom time.Time
}

func (q WorklogQuery) request() (trackerapi.Request, error) {
	switch {
	case q.Author == "":
		return trackerapi.Request{}, errors.New("the query names no author")
	case q.From.IsZero() || q.To.IsZero():
		return trackerapi.Request{}, errors.New("the query needs both a From and a To date")
	case q.To.Before(q.From):
		return trackerapi.Request{}, fmt.Errorf("the query ends on %s, before it starts on %s", q.To, q.From)
	}
	query := url.Values{"from": {q.From.String()}, "to": {q.To.String()}}
	if !q.UpdatedFrom.IsZero() {
		query.Set("updatedFrom", q.UpdatedFrom.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return trackerapi.Request{Path: path("4/worklogs/user", q.Author), Query: query}, nil
}

// WorklogPages reads q's worklogs a page at a time and hands each page to
// fn in Tempo's order. A caller that keeps only what it needs from each page
// bounds its memory by one page. An error from fn ends the listing and
// is returned as is; a failure on a later page comes after fn has seen
// the earlier ones, and so does a page drawn under another credential
// read than the first, which fails the listing with an error matching
// trackerapi.ErrCredentialChanged (Worklogs reads again instead).
func (c *Client) WorklogPages(ctx context.Context, q WorklogQuery, fn func([]Worklog) error) error {
	if err := c.admit(); err != nil {
		return err
	}
	r, err := q.request()
	if err != nil {
		return trackerapi.InvalidInput(fmt.Errorf("tempo: list worklogs: %w", err))
	}
	var fnErr error
	err = pages(ctx, c.http, r, func(resp *trackerapi.Response, raw []worklogJSON) error {
		list := make([]Worklog, len(raw))
		for i, w := range raw {
			if list[i], err = w.worklog(resp); err != nil {
				return err
			}
		}
		if err := fn(list); err != nil {
			fnErr = err
			return err
		}
		return nil
	})
	if err != nil && fnErr == nil {
		return fmt.Errorf("tempo: list worklogs of %s from %s to %s: %w", q.Author, q.From, q.To, err)
	}
	return err
}

// Worklogs reads all of q's worklogs. It holds the whole range in memory;
// WorklogPages bounds memory to a page. A failure returns no worklogs,
// since a part of the range would read as all of it; WorklogPages hands
// over each page as it is read.
func (c *Client) Worklogs(ctx context.Context, q WorklogQuery) ([]Worklog, error) {
	out, err := walk(ctx, func(ctx context.Context) ([]Worklog, error) {
		var out []Worklog
		err := c.WorklogPages(ctx, q, func(page []Worklog) error {
			out = append(out, page...)
			return nil
		})
		return out, err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Worklog reads one worklog by its Tempo ID. A worklog that does not
// exist fails with an error matching trackerapi.ErrNotFound.
func (c *Client) Worklog(ctx context.Context, id int64) (Worklog, error) {
	if err := c.admit(); err != nil {
		return Worklog{}, err
	}
	if id <= 0 {
		return Worklog{}, trackerapi.InvalidInput(fmt.Errorf("tempo: read worklog: %d is not a worklog ID", id))
	}
	return c.worklog(ctx, id)
}

// worklog reads worklog id.
func (c *Client) worklog(ctx context.Context, id int64) (Worklog, error) {
	var raw worklogJSON
	resp, err := c.http.JSON(ctx, trackerapi.Request{Path: path("4/worklogs", strconv.FormatInt(id, 10))}, &raw)
	if err != nil {
		return Worklog{}, fmt.Errorf("tempo: read worklog %d: %w", id, err)
	}
	w, err := raw.worklog(resp)
	if err != nil {
		return Worklog{}, fmt.Errorf("tempo: read worklog %d: %w", id, err)
	}
	return w, nil
}

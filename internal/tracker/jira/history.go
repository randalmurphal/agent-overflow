package jira

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// The bulk changelog fetch takes at most this many issues and field IDs.
const (
	changelogIssueLimit = 1000
	changelogFieldLimit = 10
)

// ChangeEntry is one change to an issue: who made it, when, and each
// field it changed.
type ChangeEntry struct {
	ID      string
	Author  *User // nil for a change Jira made itself
	Created time.Time
	Items   []ChangeItem
}

// ChangeItem is one field's change. From and To are the raw values (IDs,
// account IDs, numbers), empty when the field had none; FromText and
// ToText are what a person reads.
type ChangeItem struct {
	FieldID   string
	Field     string // the field's name
	FieldType string // jira or custom
	From      string
	To        string
	FromText  string
	ToText    string
}

type changeItemJSON struct {
	FieldID   string  `json:"fieldId"`
	Field     string  `json:"field"`
	FieldType string  `json:"fieldtype"`
	From      *string `json:"from"`
	To        *string `json:"to"`
	FromText  *string `json:"fromString"`
	ToText    *string `json:"toString"`
}

type changeEntryJSON struct {
	ID      string           `json:"id"`
	Author  *userJSON        `json:"author"`
	Created changeTime       `json:"created"`
	Items   []changeItemJSON `json:"items"`
}

func (e changeEntryJSON) entry() ChangeEntry {
	out := ChangeEntry{ID: e.ID, Author: e.Author.userPtr(), Created: time.Time(e.Created)}
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	for _, it := range e.Items {
		out.Items = append(out.Items, ChangeItem{
			FieldID: it.FieldID, Field: it.Field, FieldType: it.FieldType,
			From: deref(it.From), To: deref(it.To), FromText: deref(it.FromText), ToText: deref(it.ToText),
		})
	}
	return out
}

// changeTime reads a change's time: a Jira timestamp, as the per-issue
// changelog sends it, or milliseconds since the epoch, as the bulk
// fetch's published example does.
type changeTime time.Time

func (t *changeTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if isNull(b) {
		return nil
	}
	if b[0] != '"' {
		ms, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return fmt.Errorf("a change time of %s", b)
		}
		*t = changeTime(time.UnixMilli(ms))
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := parseTime(s)
	if err != nil {
		return err
	}
	*t = changeTime(parsed)
	return nil
}

// Changelog is an issue's changes, oldest first. Complete is false when
// the walk stopped early.
type Changelog struct {
	Entries  []ChangeEntry
	Complete bool
}

// Changelog reads every change to an issue, page by page. A failure after
// some pages returns them with the error.
func (c *Client) Changelog(ctx context.Context, issue string) (Changelog, error) {
	if err := c.admit(ctx); err != nil {
		return Changelog{}, err
	}
	path, err := issuePath(issue)
	if err != nil {
		return Changelog{}, err
	}
	found, complete, err := offsetPages[changeEntryJSON](ctx, c, apiPath+path+"/changelog", url.Values{}, 100, 0)
	out := Changelog{Complete: complete && err == nil}
	for _, e := range found {
		out.Entries = append(out.Entries, e.entry())
	}
	if err != nil {
		return out, fmt.Errorf("jira: changelog of %s: %w", issue, err)
	}
	return out, nil
}

// ChangelogQuery names the issues, by key or ID, whose changes a bulk
// fetch reads, and the field IDs whose changes it keeps (none keeps all).
type ChangelogQuery struct {
	Issues []string
	Fields []string
}

// BulkChangelog is the changes of several issues by issue ID, each
// issue's oldest first, an entry holding the items of the fields asked
// for. Complete is false when a request failed.
type BulkChangelog struct {
	ByIssue  map[string][]ChangeEntry
	Complete bool
}

// BulkChangelogs reads the changes of many issues. Jira takes 1000 issues
// and 10 field IDs a request, so more are split across requests (every
// group of issues with every group of fields), each read to its last
// page, and an entry split across field groups is joined again. A
// failure returns what was read with the error; a repeated page token or
// an empty page that names a next one fails it rather than loop.
func (c *Client) BulkChangelogs(ctx context.Context, q ChangelogQuery) (BulkChangelog, error) {
	out := BulkChangelog{ByIssue: map[string][]ChangeEntry{}}
	if err := c.admit(ctx); err != nil {
		return out, err
	}
	if len(q.Issues) == 0 {
		return out, notSent("a changelog fetch needs issues")
	}
	for _, ref := range q.Issues {
		if err := checkIssue(ref); err != nil {
			return out, err
		}
	}
	for _, f := range q.Fields {
		if strings.TrimSpace(f) == "" {
			return out, notSent("a changelog fetch with an empty field ID")
		}
	}
	fieldGroups := [][]string{nil}
	if len(q.Fields) > 0 {
		fieldGroups = slices.Collect(slices.Chunk(q.Fields, changelogFieldLimit))
	}
	merged := map[string]map[string]*ChangeEntry{}
	var err error
	for issues := range slices.Chunk(q.Issues, changelogIssueLimit) {
		for _, fields := range fieldGroups {
			if err = c.changelogPages(ctx, issues, fields, merged); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	for id, byID := range merged {
		entries := make([]ChangeEntry, 0, len(byID))
		for _, e := range byID {
			entries = append(entries, *e)
		}
		slices.SortFunc(entries, func(a, b ChangeEntry) int {
			if c := a.Created.Compare(b.Created); c != 0 {
				return c
			}
			return cmp.Compare(numericOrder(a.ID), numericOrder(b.ID))
		})
		out.ByIssue[id] = entries
	}
	if err != nil {
		return out, fmt.Errorf("jira: bulk changelogs: %w", err)
	}
	out.Complete = true
	return out, nil
}

func numericOrder(id string) int64 {
	n, _ := strconv.ParseInt(id, 10, 64)
	return n
}

// changelogPages reads one bulk request to its last page into merged, by
// issue ID and entry ID; an entry seen again adds the items it lacks.
func (c *Client) changelogPages(ctx context.Context, issues, fields []string, merged map[string]map[string]*ChangeEntry) error {
	seen := map[string]bool{}
	for token := ""; ; {
		body := map[string]any{"issueIdsOrKeys": issues, "maxResults": changelogIssueLimit}
		if len(fields) > 0 {
			body["fieldIds"] = fields
		}
		if token != "" {
			body["nextPageToken"] = token
		}
		var page struct {
			Logs []struct {
				IssueID string            `json:"issueId"`
				Changes []changeEntryJSON `json:"changeHistories"`
			} `json:"issueChangeLogs"`
			NextPageToken string `json:"nextPageToken"`
		}
		_, err := c.call(ctx, trackerapi.Request{Method: http.MethodPost, Path: apiPath + "changelog/bulkfetch", Body: body, ReadOnly: true}, &page)
		if err != nil {
			return err
		}
		n := 0
		for _, l := range page.Logs {
			byID := merged[l.IssueID]
			if byID == nil {
				byID = map[string]*ChangeEntry{}
				merged[l.IssueID] = byID
			}
			for _, j := range l.Changes {
				n++
				e := j.entry()
				have, ok := byID[e.ID]
				if !ok {
					byID[e.ID] = &e
					continue
				}
				for _, it := range e.Items {
					if !slices.Contains(have.Items, it) {
						have.Items = append(have.Items, it)
					}
				}
			}
		}
		switch {
		case page.NextPageToken == "":
			return nil
		case n == 0:
			return fmt.Errorf("an empty page names a next page")
		case seen[page.NextPageToken]:
			return fmt.Errorf("the page token repeated")
		}
		seen[page.NextPageToken] = true
		token = page.NextPageToken
	}
}

// StatusChange reads a status change: the status IDs before and after.
// ok is false for an item of another field.
func (it ChangeItem) StatusChange() (from, to string, ok bool) {
	if it.FieldID != "status" {
		return "", "", false
	}
	return it.From, it.To, true
}

// AssigneeChange reads an assignee change: the account IDs before and
// after, empty for unassigned. ok is false for an item of another field.
func (it ChangeItem) AssigneeChange() (from, to string, ok bool) {
	if it.FieldID != "assignee" {
		return "", "", false
	}
	return it.From, it.To, true
}

// SprintIDs reads an item of a sprint field, whose ID varies by site:
// the IDs of the sprints the issue was in before and after the change,
// each sorted and without repeats.
func (it ChangeItem) SprintIDs() (from, to []int64, err error) {
	if from, err = ParseSprintIDs(it.From); err != nil {
		return nil, nil, err
	}
	if to, err = ParseSprintIDs(it.To); err != nil {
		return nil, nil, err
	}
	return from, to, nil
}

// ParseSprintIDs reads the comma-separated sprint IDs a sprint field's
// change holds: spaces and empty entries are skipped, the IDs are sorted
// and each appears once. Anything but a positive number is an error.
func ParseSprintIDs(s string) ([]int64, error) {
	var out []int64
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("jira: %q in a sprint change is not a sprint ID", part)
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// FieldChange is one change to one field, with when and who.
type FieldChange struct {
	When   time.Time
	Author *User
	Item   ChangeItem
}

// FieldChanges picks the changes to the field with fieldID (an estimate's
// field ID, say) from entries, in their order.
func FieldChanges(entries []ChangeEntry, fieldID string) []FieldChange {
	var out []FieldChange
	for _, e := range entries {
		for _, it := range e.Items {
			if it.FieldID == fieldID {
				out = append(out, FieldChange{When: e.Created, Author: e.Author, Item: it})
			}
		}
	}
	return out
}

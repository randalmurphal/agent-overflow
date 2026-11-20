package trackerfake

import (
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// pageToken is the opaque token of the page from offset of a token-paged
// read; it names what it pages (the JQL and list), so a token handed to
// another read is refused.
func pageToken(scope string, offset int) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(scope))
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d:%08x", offset, h.Sum32()))
}

// readPageToken is the offset a token names; empty is the first page.
func readPageToken(scope, token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	num, _, ok := strings.Cut(string(raw), ":")
	offset, perr := strconv.Atoi(num)
	if err != nil || !ok || perr != nil || pageToken(scope, offset) != token {
		return 0, errors.New("invalid page token")
	}
	return offset, nil
}

// tokenPage is a token-paged page of issues from offset: the page, the
// next token and whether it is the last.
func (c *jiraCall) tokenPage(issues []*JiraIssue, scope string, offset, size int, sel fieldSelection) ([]any, string, bool) {
	page, next := tokenWindow(issues, scope, offset, size)
	out := []any{}
	for _, is := range page {
		out = append(out, c.s.issueJSON(is, sel, c.viewer))
	}
	return out, next, next == ""
}

// searchSize is how many issues a search page holds: up to 5000 asked
// for, at most 100 when it returns fields beyond the ID (measured: 100
// for fields=summary and 5000 for fields=id at maxResults=5000). A search
// that matches nothing, a missing key or project included, answers 200
// with no issues and no warnings member (measured).
func searchSize(asked int, sel fieldSelection, s *jiraSite) int {
	if asked <= 0 {
		asked = 50
	}
	if len(sel.fieldIDs(s)) > 0 {
		return min(asked, 100)
	}
	return min(asked, 5000)
}

// jqlAnswer is Jira's refusal of a JQL it cannot read, or the fake's
// unhandled answer for a form it does not implement.
func jqlAnswer(err error) answer {
	var je *jqlError
	if errors.As(err, &je) {
		return unhandledf("%v", err)
	}
	return jiraErrors(http.StatusBadRequest, err.Error())
}

func searchJQL(c *jiraCall) answer {
	var in struct {
		JQL           string   `json:"jql"`
		MaxResults    int      `json:"maxResults"`
		Fields        []string `json:"fields"`
		NextPageToken string   `json:"nextPageToken"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	q, err := parseJQL(in.JQL)
	if err != nil {
		return jqlAnswer(err)
	}
	offset, err := readPageToken("search "+in.JQL, in.NextPageToken)
	if err != nil {
		return jiraErrors(http.StatusBadRequest, "The provided next page token is invalid or expired.")
	}
	sel := parseFields(in.Fields, selectNone)
	issues, next, last := c.tokenPage(c.s.search(q), "search "+in.JQL, offset, searchSize(in.MaxResults, sel, c.s), sel)
	out := map[string]any{"issues": issues, "isLast": last}
	if next != "" {
		out["nextPageToken"] = next
	}
	return jsonAnswer(http.StatusOK, out)
}

func countJQL(c *jiraCall) answer {
	var in struct {
		JQL string `json:"jql"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	q, err := parseJQL(in.JQL)
	if err != nil {
		return jqlAnswer(err)
	}
	return jsonAnswer(http.StatusOK, map[string]any{"count": len(c.s.search(q))})
}

func bulkFetch(c *jiraCall) answer {
	var in struct {
		IssueIDsOrKeys []string `json:"issueIdsOrKeys"`
		Fields         []string `json:"fields"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	if len(in.IssueIDsOrKeys) == 0 || len(in.IssueIDsOrKeys) > 100 {
		return jiraErrors(http.StatusBadRequest, "The number of issues must be between 1 and 100.")
	}
	sel := parseFields(in.Fields, selectNavigable)
	issues := []any{}
	seen := map[string]bool{}
	for _, ref := range in.IssueIDsOrKeys {
		if is := c.s.issue(ref); is != nil && !seen[is.ID] {
			seen[is.ID] = true
			issues = append(issues, c.s.issueJSON(is, sel, c.viewer))
		}
	}
	return jsonAnswer(http.StatusOK, map[string]any{"expand": "", "issues": issues, "issueErrors": []any{}})
}

func issueChangelog(c *jiraCall) answer {
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	start, size, err := c.pageWindow(100, 100)
	if err != nil {
		return unhandledf("%v", err)
	}
	values := make([]any, len(is.Changelog))
	for i, ch := range is.Changelog {
		values[i] = c.s.changelogJSON(ch, false)
	}
	page := offsetPage(values, start, size)
	page["self"] = c.s.self("issue/" + is.Key + "/changelog")
	return jsonAnswer(http.StatusOK, page)
}

func bulkChangelog(c *jiraCall) answer {
	var in struct {
		IssueIDsOrKeys []string `json:"issueIdsOrKeys"`
		FieldIDs       []string `json:"fieldIds"`
		MaxResults     int      `json:"maxResults"`
		NextPageToken  string   `json:"nextPageToken"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	switch {
	case len(in.IssueIDsOrKeys) == 0 || len(in.IssueIDsOrKeys) > 1000:
		return jiraErrors(http.StatusBadRequest, "The number of issues must be between 1 and 1000.")
	case len(in.FieldIDs) > 10:
		return jiraErrors(http.StatusBadRequest, "The number of field IDs must be at most 10.")
	}
	scope := "changelog " + strings.Join(in.IssueIDsOrKeys, ",") + " " + strings.Join(in.FieldIDs, ",")
	offset, err := readPageToken(scope, in.NextPageToken)
	if err != nil {
		return jiraErrors(http.StatusBadRequest, "The provided next page token is invalid or expired.")
	}
	size := in.MaxResults
	if size <= 0 || size > 10000 {
		size = 1000
	}
	type history struct {
		issue string
		entry map[string]any
	}
	var all []history
	seen := map[string]bool{}
	for _, ref := range in.IssueIDsOrKeys {
		is := c.s.issue(ref)
		if is == nil || seen[is.ID] {
			continue
		}
		seen[is.ID] = true
		for _, ch := range is.Changelog {
			if len(in.FieldIDs) > 0 {
				ch.Items = slices.DeleteFunc(slices.Clone(ch.Items), func(it JiraChangeItem) bool { return !slices.Contains(in.FieldIDs, it.FieldID) })
			}
			if len(ch.Items) > 0 {
				all = append(all, history{is.ID, c.s.changelogJSON(ch, true)})
			}
		}
	}
	page, next := tokenWindow(all, scope, offset, size)
	var logs []map[string]any
	for _, h := range page {
		if n := len(logs); n > 0 && logs[n-1]["issueId"] == h.issue {
			logs[n-1]["changeHistories"] = append(logs[n-1]["changeHistories"].([]any), h.entry)
			continue
		}
		logs = append(logs, map[string]any{"issueId": h.issue, "changeHistories": []any{h.entry}})
	}
	out := map[string]any{"issueChangeLogs": orEmpty(logs)}
	if next != "" {
		out["nextPageToken"] = next
	}
	return jsonAnswer(http.StatusOK, out)
}

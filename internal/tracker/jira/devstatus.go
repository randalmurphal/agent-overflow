package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// The development panel's endpoints (dev-status) are the ones Jira's web
// UI reads for an issue's branches and pull requests from a forge
// integration. They are undocumented and can change without notice; the
// shapes read here are the measured ones
// (docs/references/tracker-api-measurements.md, "Development data").
const devStatusPath = "rest/dev-status/latest/issue/"

// devStatusErrorBytes bounds each element of an answer's errors that a
// DevStatusError quotes.
const devStatusErrorBytes = 300

// DevStatusError is a development panel answer whose errors or
// configErrors name something: the panel could not read the integration.
// Errors holds each element as the answer gave it, each cut to
// devStatusErrorBytes; no element has been measured, so none is read
// for its fields.
type DevStatusError struct {
	Path   string
	Errors []string
}

func (e *DevStatusError) Error() string {
	return fmt.Sprintf("jira: %s answered errors: %s", e.Path, strings.Join(e.Errors, "; "))
}

// DevSummary is what the development panel counts for an issue's pull
// requests: by instance type (the integration, such as
// "oAuth-gitlab-jira-connect-gitlab.com"), how many it links. An
// integration that links none is not in PullRequests.
type DevSummary struct {
	PullRequests map[string]int
}

// devSummaryJSON is the part of the summary answer DevSummary reads.
// Measured: summary.pullrequest is present for an issue with no pull
// requests too, with overall.count 0 and an empty byInstanceType.
type devSummaryJSON struct {
	Errors       []json.RawMessage `json:"errors"`
	ConfigErrors []json.RawMessage `json:"configErrors"`
	Summary      *struct {
		PullRequest *struct {
			Overall *struct {
				Count *int `json:"count"`
			} `json:"overall"`
			ByInstanceType map[string]struct {
				Count *int `json:"count"`
			} `json:"byInstanceType"`
		} `json:"pullrequest"`
	} `json:"summary"`
}

// DevSummary reads the development panel's summary of issue, whose ID is
// the issue's number (the endpoint takes no key). One request. An answer
// without the measured pull request part is an *AnswerError, and one
// whose errors name something a *DevStatusError.
func (c *Client) DevSummary(ctx context.Context, issueID string) (DevSummary, error) {
	if err := c.admit(ctx); err != nil {
		return DevSummary{}, err
	}
	if err := checkDevIssue(issueID); err != nil {
		return DevSummary{}, err
	}
	var out devSummaryJSON
	path := devStatusPath + "summary"
	if _, err := c.call(ctx, trackerapi.Request{Path: path, Query: url.Values{"issueId": {issueID}}}, &out); err != nil {
		return DevSummary{}, err
	}
	if err := devErrors(path, out.Errors, out.ConfigErrors); err != nil {
		return DevSummary{}, err
	}
	pr := out.Summary
	if pr == nil || pr.PullRequest == nil || pr.PullRequest.Overall == nil || pr.PullRequest.Overall.Count == nil || pr.PullRequest.ByInstanceType == nil {
		return DevSummary{}, devShapeError(path, "it has no pull request count")
	}
	sum := DevSummary{PullRequests: map[string]int{}}
	for kind, n := range pr.PullRequest.ByInstanceType {
		if n.Count == nil || *n.Count < 0 {
			return DevSummary{}, devShapeError(path, fmt.Sprintf("instance type %q has no pull request count", kind))
		}
		if *n.Count > 0 {
			sum.PullRequests[kind] = *n.Count
		}
	}
	return sum, nil
}

// DevPullRequest is a pull request the development panel links to an
// issue, as an integration reported it. Measured on the GitLab
// integration: URL is the merge request's page
// (https://gitlab.com/<group>[/<subgroup>]/<project>/-/merge_requests/<n>),
// ID is "<group>[/<subgroup>]/<project>!<n>", and Status is MERGED,
// OPEN or DECLINED; other values are unmeasured.
type DevPullRequest struct {
	ID     string
	Name   string
	URL    string
	Status string
	// Author is the author's name as the integration reports it; empty
	// when it reports none.
	Author string
	// Branch is the source branch.
	Branch string
}

type devDetailJSON struct {
	Errors []json.RawMessage `json:"errors"`
	Detail *[]struct {
		PullRequests *[]struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			URL    string `json:"url"`
			Status string `json:"status"`
			Author *struct {
				Name string `json:"name"`
			} `json:"author"`
			Source *struct {
				Branch string `json:"branch"`
			} `json:"source"`
		} `json:"pullRequests"`
	} `json:"detail"`
}

// DevPullRequests reads the pull requests instanceType links to issue
// (a DevSummary key), in one request. The answer also carries the
// integration's branches, which are not read. A pull request without a
// URL or a status, or an answer without the measured detail list, is an
// *AnswerError; one whose errors name something a *DevStatusError.
func (c *Client) DevPullRequests(ctx context.Context, issueID, instanceType string) ([]DevPullRequest, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if err := checkDevIssue(issueID); err != nil {
		return nil, err
	}
	if instanceType == "" {
		return nil, &trackerapi.NotSentError{Err: fmt.Errorf("jira: a development detail read names its instance type")}
	}
	var out devDetailJSON
	path := devStatusPath + "detail"
	q := url.Values{"issueId": {issueID}, "applicationType": {instanceType}, "dataType": {"pullrequest"}}
	if _, err := c.call(ctx, trackerapi.Request{Path: path, Query: q}, &out); err != nil {
		return nil, err
	}
	if err := devErrors(path, out.Errors); err != nil {
		return nil, err
	}
	if out.Detail == nil {
		return nil, devShapeError(path, "it has no detail list")
	}
	var prs []DevPullRequest
	for _, d := range *out.Detail {
		if d.PullRequests == nil {
			return nil, devShapeError(path, "an instance has no pull request list")
		}
		for _, pr := range *d.PullRequests {
			if pr.URL == "" || pr.Status == "" {
				return nil, devShapeError(path, "a pull request has no link or no status")
			}
			out := DevPullRequest{ID: pr.ID, Name: pr.Name, URL: pr.URL, Status: pr.Status}
			if pr.Author != nil {
				out.Author = pr.Author.Name
			}
			if pr.Source != nil {
				out.Branch = pr.Source.Branch
			}
			prs = append(prs, out)
		}
	}
	return prs, nil
}

// checkDevIssue refuses an issue ID that is not a number before
// anything is sent: the endpoints take the issue's number, not its key.
func checkDevIssue(id string) error {
	if id == "" || strings.Trim(id, "0123456789") != "" {
		return &trackerapi.NotSentError{Err: fmt.Errorf("jira: the development panel takes an issue's number, not %q", id)}
	}
	return nil
}

// devErrors is a *DevStatusError for the elements lists name, nil when
// every list is empty.
func devErrors(path string, lists ...[]json.RawMessage) error {
	var quoted []string
	for _, list := range lists {
		for _, raw := range list {
			text := string(raw)
			if len(text) > devStatusErrorBytes {
				text = text[:devStatusErrorBytes] + "…"
			}
			quoted = append(quoted, text)
		}
	}
	if len(quoted) == 0 {
		return nil
	}
	return &DevStatusError{Path: path, Errors: quoted}
}

// devShapeError is an answer of path that is not the measured shape.
func devShapeError(path, why string) error {
	return &AnswerError{Message: fmt.Sprintf("%s answered in a shape this app does not know (%s); the endpoint is undocumented and may have changed", path, why)}
}

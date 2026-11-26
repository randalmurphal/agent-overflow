package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
// DevStatusError quotes, and devStatusErrorsQuoted how many it quotes.
const (
	devStatusErrorBytes   = 300
	devStatusErrorsQuoted = 3
)

// DevCallError is a failure of one development panel read: Path is the
// endpoint, InstanceType the integration a detail read asked for (empty
// for the summary). Every failure of DevSummary and DevPullRequests is
// one.
type DevCallError struct {
	Path         string
	InstanceType string
	Err          error
}

func (e *DevCallError) Error() string {
	if e.InstanceType == "" {
		return fmt.Sprintf("jira: %s: %v", e.Path, e.Err)
	}
	return fmt.Sprintf("jira: %s for %s: %v", e.Path, e.InstanceType, e.Err)
}

func (e *DevCallError) Unwrap() error { return e.Err }

// DevAbsentError is a summary that says the site keeps no development
// data: a 404, or an answer with no pull request part. Neither is
// measured; a site with no forge integration may answer either.
type DevAbsentError struct{ Why string }

func (e *DevAbsentError) Error() string {
	return "the development panel holds no pull request data (" + e.Why + ")"
}

// DevStatusError is a development panel answer whose errors or
// configErrors name something: the panel could not read the integration.
// Errors holds the first devStatusErrorsQuoted elements as the answer
// gave them, each cut to devStatusErrorBytes, and More counts the rest;
// no element has been measured, so none is read for its fields.
type DevStatusError struct {
	Errors []string
	More   int
}

func (e *DevStatusError) Error() string {
	text := "answered errors: " + strings.Join(e.Errors, "; ")
	if e.More > 0 {
		text += fmt.Sprintf(" (and %d more)", e.More)
	}
	return text
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
// the issue's number (the endpoint takes no key). One request. Its
// failure is a *DevCallError: holding a *DevAbsentError for a 404 or an
// answer with no pull request part, a *DevShapeError for a pull request
// part not as measured, and a *DevStatusError for errors the answer
// names.
func (c *Client) DevSummary(ctx context.Context, issueID string) (DevSummary, error) {
	path := devStatusPath + "summary"
	sum, err := c.devSummary(ctx, path, issueID)
	if err != nil {
		return DevSummary{}, &DevCallError{Path: path, Err: err}
	}
	return sum, nil
}

func (c *Client) devSummary(ctx context.Context, path, issueID string) (DevSummary, error) {
	if err := c.admit(ctx); err != nil {
		return DevSummary{}, err
	}
	if err := checkID("issue", issueID); err != nil {
		return DevSummary{}, err
	}
	var out devSummaryJSON
	if _, err := c.call(ctx, trackerapi.Request{Path: path, Query: url.Values{"issueId": {issueID}}}, &out); err != nil {
		// A 404 the sign-in check confirmed (checkNotFound) is the
		// answer itself; one it could not confirm, or that the
		// credential caused, is another error.
		if status, ok := err.(*trackerapi.StatusError); ok && status.Status == http.StatusNotFound {
			return DevSummary{}, &DevAbsentError{Why: "HTTP 404"}
		}
		return DevSummary{}, err
	}
	if err := devErrors(out.Errors, out.ConfigErrors); err != nil {
		return DevSummary{}, err
	}
	if out.Summary == nil || out.Summary.PullRequest == nil {
		return DevSummary{}, &DevAbsentError{Why: "the summary has no pull request part"}
	}
	pr := out.Summary.PullRequest
	if pr.Overall == nil || pr.Overall.Count == nil || pr.ByInstanceType == nil {
		return DevSummary{}, &DevShapeError{Why: "it has no pull request count"}
	}
	sum := DevSummary{PullRequests: map[string]int{}}
	for kind, n := range pr.ByInstanceType {
		if n.Count == nil || *n.Count < 0 {
			return DevSummary{}, &DevShapeError{Why: fmt.Sprintf("instance type %q has no pull request count", kind)}
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
// (https://gitlab.com/<group>[/<subgroup>]/<project>/-/merge_requests/<n>)
// and Status is MERGED, OPEN or DECLINED; other values are unmeasured.
// Either is empty when the integration gave none.
type DevPullRequest struct {
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
// integration's branches, which are not read. A pull request with no URL
// or status is returned as given, for the caller to judge beside the
// others. Its failure is a *DevCallError: holding a *DevShapeError for
// an answer without the measured detail list, and a *DevStatusError for
// errors the answer names.
func (c *Client) DevPullRequests(ctx context.Context, issueID, instanceType string) ([]DevPullRequest, error) {
	path := devStatusPath + "detail"
	prs, err := c.devPullRequests(ctx, path, issueID, instanceType)
	if err != nil {
		return nil, &DevCallError{Path: path, InstanceType: instanceType, Err: err}
	}
	return prs, nil
}

func (c *Client) devPullRequests(ctx context.Context, path, issueID, instanceType string) ([]DevPullRequest, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if err := checkID("issue", issueID); err != nil {
		return nil, err
	}
	if instanceType == "" {
		return nil, notSent("a development detail read names its instance type")
	}
	var out devDetailJSON
	q := url.Values{"issueId": {issueID}, "applicationType": {instanceType}, "dataType": {"pullrequest"}}
	if _, err := c.call(ctx, trackerapi.Request{Path: path, Query: q}, &out); err != nil {
		return nil, err
	}
	if err := devErrors(out.Errors); err != nil {
		return nil, err
	}
	if out.Detail == nil {
		return nil, &DevShapeError{Why: "it has no detail list"}
	}
	var prs []DevPullRequest
	for _, d := range *out.Detail {
		if d.PullRequests == nil {
			return nil, &DevShapeError{Why: "an instance has no pull request list"}
		}
		for _, pr := range *d.PullRequests {
			out := DevPullRequest{Name: pr.Name, URL: pr.URL, Status: pr.Status}
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

// devErrors is a *DevStatusError for the elements lists name, nil when
// every list is empty.
func devErrors(lists ...[]json.RawMessage) error {
	var dev DevStatusError
	for _, list := range lists {
		for _, raw := range list {
			if len(dev.Errors) == devStatusErrorsQuoted {
				dev.More++
				continue
			}
			text := string(raw)
			if len(text) > devStatusErrorBytes {
				text = text[:devStatusErrorBytes] + "…"
			}
			dev.Errors = append(dev.Errors, text)
		}
	}
	if len(dev.Errors) == 0 {
		return nil
	}
	return &dev
}

// DevShapeError is a development panel answer that is not in the
// measured shape: Why says what it lacks. The endpoints are undocumented,
// so this is the error a change of them shows as.
type DevShapeError struct {
	Why string
}

func (e *DevShapeError) Error() string {
	return fmt.Sprintf("answered in a shape this app does not know (%s); the endpoint is undocumented and may have changed", e.Why)
}

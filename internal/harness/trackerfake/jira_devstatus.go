package trackerfake

import (
	"net/http"
	"slices"
)

// Jira's development panel (rest/dev-status/latest/issue/...), which the
// web UI reads and Atlassian does not document. Measured, read-only, on
// one site with the GitLab.com integration
// (docs/references/tracker-api-measurements.md, "Development data"): the
// summary answers 200 with errors, configErrors and
// summary.pullrequest.{overall.count, byInstanceType.<type>.count}, the
// count 0 and byInstanceType empty for an issue with no pull request; the
// detail answers 200 with errors and detail[].pullRequests[], the same
// body for dataType pullrequest and branch. The fake answers the keys the
// app reads; the other measured keys hold values it does not model and
// are left out. Unmeasured, and unhandled here: an issue the site lacks
// or the caller may not see, a detail for an instance type that links no
// pull request to the issue, a site with no integration, and a caller
// without the "View development tools" permission.

func devSummary(c *jiraCall) answer {
	if err := c.noBody(); err != nil {
		return unhandledf("%v", err)
	}
	is := c.s.issue(c.query.Get("issueId"))
	if is == nil || is.ID != c.query.Get("issueId") {
		return unhandledf("dev-status summary of %q: an issue the site lacks is unmeasured", c.query.Get("issueId"))
	}
	byType := map[string]any{}
	for _, pr := range is.PullRequests {
		n, _ := byType[pr.InstanceType].(map[string]any)
		if n == nil {
			n = map[string]any{"count": 0}
			byType[pr.InstanceType] = n
		}
		n["count"] = n["count"].(int) + 1
	}
	return jsonAnswer(http.StatusOK, map[string]any{
		"errors": []any{}, "configErrors": []any{},
		"summary": map[string]any{
			"pullrequest": map[string]any{"overall": map[string]any{"count": len(is.PullRequests)}, "byInstanceType": byType},
		},
	})
}

func devDetail(c *jiraCall) answer {
	if err := c.noBody(); err != nil {
		return unhandledf("%v", err)
	}
	is := c.s.issue(c.query.Get("issueId"))
	if is == nil || is.ID != c.query.Get("issueId") {
		return unhandledf("dev-status detail of %q: an issue the site lacks is unmeasured", c.query.Get("issueId"))
	}
	// Measured: dataType branch answers the same body as pullrequest.
	if dt := c.query.Get("dataType"); dt != "pullrequest" && dt != "branch" {
		return unhandledf("dev-status detail dataType=%q", dt)
	}
	kind := c.query.Get("applicationType")
	if !slices.ContainsFunc(is.PullRequests, func(pr JiraPullRequest) bool { return pr.InstanceType == kind }) {
		return unhandledf("dev-status detail of %s for %q, which links no pull request to it, is unmeasured", is.Key, kind)
	}
	prs := []any{}
	for _, pr := range is.PullRequests {
		if pr.InstanceType != kind {
			continue
		}
		prs = append(prs, map[string]any{
			"id": pr.ID, "name": pr.Name, "url": pr.URL, "status": pr.Status,
			"author": map[string]any{"name": pr.Author}, "source": map[string]any{"branch": pr.Branch},
		})
	}
	return jsonAnswer(http.StatusOK, map[string]any{
		"errors": []any{},
		"detail": []any{map[string]any{"branches": []any{}, "pullRequests": prs, "repositories": []any{}, "_instance": map[string]any{"type": kind}}},
	})
}

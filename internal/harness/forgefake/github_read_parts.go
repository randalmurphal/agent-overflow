package forgefake

import (
	"errors"
	"fmt"
	"slices"
)

// The PR read's optional parts (the app's githubReadParts): the @include
// variables PRTick and its page operations gate the management fields,
// team reviewers, check runs and workflow runs with, and $pageSize, the
// page of threads and comments. Repo.TeamsRefused and Repo.TokenCannotRead
// make a read that selects a part refuse it as GitHub refuses a token
// that cannot read it.

// ghReadParts are a read's optional parts and page size.
type ghReadParts struct {
	pageSize                               int
	manage, teams, checkRuns, workflowRuns bool
}

// readParts reads the part variables names; a part the document does not
// gate is selected, and the page size is githubGraphQLPageSize.
func (v graphQLVars) readParts(names ...string) (ghReadParts, error) {
	out := ghReadParts{pageSize: githubGraphQLPageSize, manage: true, teams: true, checkRuns: true, workflowRuns: true}
	for _, name := range names {
		var err error
		switch name {
		case "pageSize":
			if out.pageSize, err = v.integer(name); err == nil && (out.pageSize < 1 || out.pageSize > githubGraphQLPageSize) {
				err = fmt.Errorf("variable pageSize %d is not 1 to %d", out.pageSize, githubGraphQLPageSize)
			}
		case "wantManage":
			out.manage, err = v.boolean(name)
		case "wantTeams":
			out.teams, err = v.boolean(name)
		case "wantCheckRuns":
			out.checkRuns, err = v.boolean(name)
		case "wantWorkflowRuns":
			out.workflowRuns, err = v.boolean(name)
		default:
			err = fmt.Errorf("%q is not a read part", name)
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// What a fine-grained token may be unable to read (Repo.TokenCannotRead):
// check runs (fine-grained tokens have no Checks permission) and workflow
// runs (the Actions permission).
const (
	tokenCannotReadChecks  = "checks"
	tokenCannotReadActions = "actions"
)

func (r *Repo) normalizeTokenCannotRead() error {
	if r.Forge != "github" && len(r.TokenCannotRead) > 0 {
		return errors.New("tokenCannotRead names what a GitHub token cannot read")
	}
	for _, what := range r.TokenCannotRead {
		if what != tokenCannotReadChecks && what != tokenCannotReadActions {
			return fmt.Errorf("tokenCannotRead %q is not %s or %s", what, tokenCannotReadChecks, tokenCannotReadActions)
		}
	}
	return nil
}

// githubFineGrainedRefusal is GitHub's message for a field a fine-grained
// token's permissions do not cover.
const githubFineGrainedRefusal = "Resource not accessible by personal access token"

// ghTeamsScopeRefusal is GitHub's answer to a document selecting a team's
// combinedSlug with a token without read:org: no data, nothing run.
func ghTeamsScopeRefusal() response {
	return jsonResponse(map[string]any{"errors": []map[string]any{{
		"type":    "INSUFFICIENT_SCOPES",
		"message": "Your token has not been granted the required scopes to execute this query. The 'combinedSlug' field requires one of the following scopes: ['read:org'], but your token has only been granted the: ['repo'] scopes. Please modify your token's scopes at: https://github.com/settings/tokens.",
	}}})
}

// refuseContexts nulls what the token cannot read in one page of rollup
// contexts, as GitHub nulls a field it refuses, and answers an error for
// each at path, the page's nodes list: a check run, or its check suite's
// workflow run.
func (r *Repo) refuseContexts(page []map[string]any, parts ghReadParts, path []any) []map[string]any {
	var errs []map[string]any
	at := func(more ...any) []any { return append(slices.Clone(path), more...) }
	for i, node := range page {
		if node["__typename"] != "CheckRun" {
			continue
		}
		switch {
		case parts.checkRuns && slices.Contains(r.TokenCannotRead, tokenCannotReadChecks):
			page[i] = nil
			errs = append(errs, map[string]any{"type": "FORBIDDEN", "message": githubFineGrainedRefusal, "path": at(i)})
		case parts.checkRuns && parts.workflowRuns && slices.Contains(r.TokenCannotRead, tokenCannotReadActions):
			node["checkSuite"] = map[string]any{"workflowRun": nil}
			errs = append(errs, map[string]any{"type": "FORBIDDEN", "message": githubFineGrainedRefusal, "path": at(i, "checkSuite", "workflowRun")})
		}
	}
	return errs
}

// graphQLPartial is an answer with data and the errors of the fields it
// nulled; with no errors it is graphQLData.
func graphQLPartial(data map[string]any, errs []map[string]any) response {
	if len(errs) == 0 {
		return graphQLData(data)
	}
	return jsonResponse(map[string]any{"data": data, "errors": errs})
}

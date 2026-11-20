package jiraprovider

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
)

// TempoIssues is the tempo.IssueIDs a Jira connection's Tempo client is
// built with: issue keys and IDs mapped through the site's Jira client by
// bulk reads. A key that no longer names its issue (the issue moved
// project) and an issue the user cannot see are unknown.
func TempoIssues(c *jira.Client) tempo.IssueIDs { return tempoIssues{c} }

type tempoIssues struct{ jira *jira.Client }

// IDs reads keys and returns each one an issue answers to as its current
// key, case ignored.
func (t tempoIssues) IDs(ctx context.Context, keys []string) (map[string]int64, error) {
	out := map[string]int64{}
	var shaped []string
	for _, k := range keys {
		if keyShape.MatchString(k) {
			shaped = append(shaped, k)
		}
	}
	if len(shaped) == 0 {
		return out, nil
	}
	res, err := t.jira.BulkFetch(ctx, shaped, "status")
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: issue IDs for Tempo: %w", err)
	}
	for _, is := range res.Issues {
		id, err := strconv.ParseInt(is.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("jiraprovider: issue IDs for Tempo: %s has ID %q", is.Key, is.ID)
		}
		if i := slices.IndexFunc(shaped, func(k string) bool { return strings.EqualFold(k, is.Key) }); i >= 0 {
			out[shaped[i]] = id
		}
	}
	return out, nil
}

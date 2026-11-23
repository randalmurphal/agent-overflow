package linear

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// Reads of named entities: one entity by its ID, and the labels a set of
// IDs names. A caller that needs the names of labels an issue read left
// out reads exactly those, at a cost that grows with them, never a
// workspace catalog.

// maxIDs is the most IDs one by-ID request names, maxIssuePage, so a
// request is one page.
const maxIDs = maxIssuePage

// readOne reads the entity op selects at its root for id. A missing or
// invisible entity matches trackerapi.ErrNotFound.
func readOne[W, T any](ctx context.Context, c *Client, op operation, id string, typed func(W) T) (*T, error) {
	if id == "" {
		return nil, invalid(op.name, "an ID is required")
	}
	var data map[string]*W
	if _, err := c.call(ctx, op, idVars{id}, &data); err != nil {
		return nil, err
	}
	w := data[op.root]
	if w == nil {
		return nil, fmt.Errorf("linear %s: the answer holds no %s", op.name, op.root)
	}
	v := typed(*w)
	return &v, nil
}

var opComment = newQuery("Comment", "comment", `query Comment($id: String!) {
  comment(id: $id) { ...CommentFields }
}
`+commentFields)

// Comment returns one comment by its ID, with the issue it is on.
func (c *Client) Comment(ctx context.Context, id string) (*Comment, error) {
	return readOne(ctx, c, opComment, id, wireComment.typed)
}

var opRelation = newQuery("Relation", "issueRelation", `query Relation($id: String!) {
  issueRelation(id: $id) { ...RelationFields }
}
`+relationFields+issueRefFields)

// Relation returns one relation by its ID, with both its issues.
func (c *Client) Relation(ctx context.Context, id string) (*Relation, error) {
	return readOne(ctx, c, opRelation, id, wireRelation.typed)
}

var opCycle = newQuery("Cycle", "cycle", `query Cycle($id: String!) {
  cycle(id: $id) { ...CycleFields }
}
`+cycleFields)

// Cycle returns one cycle by its ID, with its team.
func (c *Client) Cycle(ctx context.Context, id string) (*Cycle, error) {
	return readOne(ctx, c, opCycle, id, wireCycle.typed)
}

var opTeam = newQuery("Team", "team", `query Team($id: String!) {
  team(id: $id) { ...TeamFields }
}
`+teamFields)

// Team returns one team by its ID; one the key cannot see matches
// trackerapi.ErrNotFound.
func (c *Client) Team(ctx context.Context, id string) (*Team, error) {
	return readOne(ctx, c, opTeam, id, wireTeam.typed)
}

type idsVars struct {
	pageVars
	IDs []string `json:"ids"`
}

// byIDs reads the entities op selects for ids, each ID once, in requests
// of at most maxIDs IDs, each paged to its end, as one trackerapi.Walk.
// An ID that names nothing visible is left out. No IDs send nothing.
func byIDs[W, T any](ctx context.Context, c *Client, op operation, ids []string, typed func(W) T) ([]T, error) {
	if err := checkIDs("ids", ids); err != nil {
		return nil, invalid(op.name, "%w", err)
	}
	var unique []string
	for _, id := range ids {
		if !slices.Contains(unique, id) {
			unique = append(unique, id)
		}
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]T, error) {
		var out []T
		for chunk := range slices.Chunk(unique, maxIDs) {
			items, err := collect(ctx, c, op, func(after string) any {
				return idsVars{pageVars{First: len(chunk), After: after}, chunk}
			}, typed)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
		return out, nil
	})
}

var opLabelsByID = newQuery("LabelsByID", "page", `query LabelsByID($ids: [ID!]!, $first: Int!, $after: String) {
  page: issueLabels(filter: { id: { in: $ids } }, includeArchived: true, first: $first, after: $after) {
    nodes { ...LabelFields } pageInfo { hasNextPage endCursor }
  }
}
`+labelFields)

// LabelsByID returns the labels ids names, archived ones included.
func (c *Client) LabelsByID(ctx context.Context, ids []string) ([]Label, error) {
	return byIDs(ctx, c, opLabelsByID, ids, wireLabel.typed)
}

var opTeamsByKey = newQuery("TeamsByKey", "page", `query TeamsByKey($filter: TeamFilter!, $first: Int!, $after: String) {
  page: teams(filter: $filter, first: $first, after: $after) {
    nodes { ...TeamFields } pageInfo { hasNextPage endCursor }
  }
}
`+teamFields)

type teamFilter struct {
	Or []teamKeyFilter `json:"or"`
}

type teamKeyFilter struct {
	Key keyCompare `json:"key"`
}

type teamsByKeyVars struct {
	pageVars
	Filter teamFilter `json:"filter"`
}

// TeamsByKey returns the teams the key can see whose key is one of keys,
// case ignored as Linear matches a team key. No keys send nothing; an
// empty key is refused before sending.
func (c *Client) TeamsByKey(ctx context.Context, keys []string) ([]Team, error) {
	var filter teamFilter
	for _, k := range keys {
		if k == "" {
			return nil, invalid(opTeamsByKey.name, "a team key is empty")
		}
		if !slices.ContainsFunc(filter.Or, func(f teamKeyFilter) bool { return strings.EqualFold(f.Key.EqIgnoreCase, k) }) {
			filter.Or = append(filter.Or, teamKeyFilter{keyCompare{EqIgnoreCase: k}})
		}
	}
	if len(filter.Or) == 0 {
		return nil, nil
	}
	first := min(len(filter.Or), maxIDs)
	return collect(ctx, c, opTeamsByKey, func(after string) any {
		return teamsByKeyVars{pageVars{First: first, After: after}, filter}
	}, wireTeam.typed)
}

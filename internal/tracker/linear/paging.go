package linear

import (
	"context"
	"fmt"
)

// pageVars are the paging variables every paged document declares as
// ($first: Int!, $after: String).
type pageVars struct {
	First int    `json:"first"`
	After string `json:"after,omitempty"`
}

// Paged documents alias the connection as "page", under a parent aliased
// as "node" when the connection hangs off one entity (a team's members,
// an issue's comments).
type rootPage[W any] struct {
	Page *connection[W] `json:"page"`
}

type nestedPage[W any] struct {
	Node *rootPage[W] `json:"node"`
}

// fetchPage requests one page of op and converts its nodes.
func fetchPage[W, T any](ctx context.Context, c *Client, op operation, vars any, nested bool, typed func(W) T) (Page[T], error) {
	var conn *connection[W]
	var usage Usage
	var err error
	if nested {
		var data nestedPage[W]
		usage, err = c.call(ctx, op, vars, &data)
		if data.Node != nil {
			conn = data.Node.Page
		}
	} else {
		var data rootPage[W]
		usage, err = c.call(ctx, op, vars, &data)
		conn = data.Page
	}
	if err != nil {
		return Page[T]{}, err
	}
	if conn == nil {
		return Page[T]{}, fmt.Errorf("linear %s: the answer holds no page", op.name)
	}
	if conn.PageInfo.HasNextPage && conn.PageInfo.EndCursor == "" {
		return Page[T]{}, fmt.Errorf("linear %s: the answer reports a next page without a cursor", op.name)
	}
	return page(*conn, usage, typed), nil
}

// collect pages op to the end, for a list whose whole size a caller
// holds anyway (teams, states, labels). varsFor builds a page's
// variables from its cursor. A failure on any page fails the call.
func collect[W, T any](ctx context.Context, c *Client, op operation, nested bool, varsFor func(after string) any, typed func(W) T) ([]T, error) {
	var out []T
	after := ""
	for {
		p, err := fetchPage(ctx, c, op, varsFor(after), nested, typed)
		if err != nil {
			if len(out) > 0 {
				return nil, fmt.Errorf("%w (%d items were read before it)", err, len(out))
			}
			return nil, err
		}
		out = append(out, p.Items...)
		if !p.HasNextPage {
			return out, nil
		}
		if p.EndCursor == after {
			return nil, fmt.Errorf("linear %s: the page cursor did not advance", op.name)
		}
		after = p.EndCursor
	}
}

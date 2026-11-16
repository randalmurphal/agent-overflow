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
func fetchPage[W, T any](ctx context.Context, c *Client, op operation, vars any, typed func(W) T) (Page[T], error) {
	var conn *connection[W]
	var r reply
	var err error
	if op.root == "node" {
		var data nestedPage[W]
		r, err = c.call(ctx, op, vars, &data)
		if data.Node != nil {
			conn = data.Node.Page
		}
	} else {
		var data rootPage[W]
		r, err = c.call(ctx, op, vars, &data)
		conn = data.Page
	}
	if err != nil {
		return Page[T]{}, err
	}
	if conn == nil {
		return Page[T]{}, fmt.Errorf("linear %s: the answer holds no page", op.name)
	}
	return page(op, *conn, r.Usage, typed)
}

// collect pages op to the end, for a list whose whole size a caller
// holds anyway (teams, states, labels). varsFor builds a page's
// variables from its cursor. A failure on any page fails the call.
func collect[W, T any](ctx context.Context, c *Client, op operation, varsFor func(after string) any, typed func(W) T) ([]T, error) {
	var out []T
	after := ""
	for {
		p, err := fetchPage(ctx, c, op, varsFor(after), typed)
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

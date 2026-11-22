package linear

import (
	"context"
	"fmt"

	"agent-overflow/internal/tracker/trackerapi"
)

// pageVars are the paging variables every paged document declares as
// ($first: Int!, $after: String).
type pageVars struct {
	First int    `json:"first"`
	After string `json:"after,omitempty"`
}

// cursor is the cursor a page was asked from; every variables type of a
// document that declares $after embeds pageVars and so has it.
func (v pageVars) cursor() string { return v.After }

// Paged documents alias the connection as "page", under a parent aliased
// as "node" when the connection hangs off one entity (a team's members,
// an issue's comments).
type rootPage[W any] struct {
	Page *connection[W] `json:"page"`
}

type nestedPage[W any] struct {
	Node *rootPage[W] `json:"node"`
}

// fetchPage requests one page of op and converts its nodes. It is the
// one place a cursor page is read, so the rule every cursor walk needs
// lives here: a page that says more follow from the very cursor it was
// asked from fails, since the next request would be this one again. A
// document that declares $after is sent only with variables that say
// which cursor they hold.
func fetchPage[W, T any](ctx context.Context, c *Client, op operation, vars any, typed func(W) T) (Page[T], error) {
	asked, cursored := vars.(interface{ cursor() string })
	if op.paged && !cursored {
		return Page[T]{}, fmt.Errorf("linear %s: the variables of a paged document hold no cursor", op.name)
	}
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
	p, err := page(op, *conn, r.Usage, typed)
	if err != nil {
		return Page[T]{}, err
	}
	if cursored && p.HasNextPage && p.EndCursor == asked.cursor() {
		return Page[T]{}, fmt.Errorf("linear %s: the page cursor did not advance", op.name)
	}
	p.Served = r.Served
	return p, nil
}

// collect pages op to the end, for a list whose whole size a caller
// holds anyway (teams, states, labels), as one trackerapi.Walk: a cursor
// continued under another key would be another user's list. varsFor
// builds a page's variables from its cursor. A failure on any page fails
// the call.
func collect[W, T any](ctx context.Context, c *Client, op operation, varsFor func(after string) any, typed func(W) T) ([]T, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]T, error) {
		return rest(ctx, c, op, varsFor, typed, Page[T]{HasNextPage: true})
	})
}

// rest is from's items and those of the pages of op that follow it, read
// from from's end cursor to the end; a from that says more follow and
// holds no cursor starts the list. A caller that holds a first page read
// another way (a composed document) continues it here, inside its own
// trackerapi.Walk.
func rest[W, T any](ctx context.Context, c *Client, op operation, varsFor func(after string) any, typed func(W) T, from Page[T]) ([]T, error) {
	out := from.Items
	for p := from; p.HasNextPage; {
		var err error
		if p, err = fetchPage(ctx, c, op, varsFor(p.EndCursor), typed); err != nil {
			if len(out) > 0 {
				return nil, fmt.Errorf("%w (%d items were read before it)", err, len(out))
			}
			return nil, err
		}
		out = append(out, p.Items...)
	}
	return out, nil
}

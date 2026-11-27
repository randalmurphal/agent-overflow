package tempo

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"strconv"

	"agent-overflow/internal/tracker/trackerapi"
)

// pageSize is the page every listing asks for. Tempo's default is 50 and
// its documented maximum 5000; 1000 worklogs stay well under the
// transport's 8 MiB body bound.
const pageSize = 1000

// page is Tempo's list envelope. next is a link to the following page and
// is empty on the last one.
type page[T any] struct {
	Metadata struct {
		Count  int    `json:"count"`
		Offset int    `json:"offset"`
		Next   string `json:"next"`
	} `json:"metadata"`
	Results []T `json:"results"`
}

// pages reads r page by page with offset and limit, handing each page's
// answer and results to fn, until Tempo names no next page. It reads the next page
// by offset rather than by following the link, so every request stays on
// the base URL. A page that names a next page but holds nothing fails the
// call instead of asking for the same offset again. A page over the
// transport's bound is asked again with half the limit
// (trackerapi.Halved), and the pages after it keep the smaller limit.
//
// Every page must be drawn under the credential read the first was: the
// token selects the tenant, so an offset continued under another token
// is another tenant's list. The pages are one trackerapi.WalkOnce: fn has
// seen the earlier pages when one is not, so the listing fails with an
// error matching trackerapi.ErrCredentialChanged, and inside a
// trackerapi.Walk that walk reads again from the start.
func pages[T any](ctx context.Context, h *trackerapi.Client, r trackerapi.Request, fn func(*trackerapi.Response, []T) error) error {
	return trackerapi.WalkOnce(ctx, func(ctx context.Context) error {
		offset, limit := 0, pageSize
		for {
			q := maps.Clone(r.Query)
			if q == nil {
				q = url.Values{}
			}
			q.Set("offset", strconv.Itoa(offset))
			q.Set("limit", strconv.Itoa(limit))
			req := r
			req.Query = q
			var p page[T]
			resp, err := h.JSON(ctx, req, &p)
			if half, ok := trackerapi.Halved(limit, err); ok {
				limit = half
				continue
			}
			if err != nil {
				return err
			}
			if err := fn(resp, p.Results); err != nil {
				return err
			}
			if p.Metadata.Next == "" {
				return nil
			}
			if len(p.Results) == 0 {
				return fmt.Errorf("the page at offset %d of %s named a next page but held no results", offset, r.Path)
			}
			offset += len(p.Results)
		}
	})
}

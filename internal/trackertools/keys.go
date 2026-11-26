package trackertools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

// Keys. A key names a ticket for thread threadID: no key, the ticket the
// thread is linked to; a key the link names, the link's (no request);
// site/KEY, that site's ticket; any other key, the ticket of the one site
// whose tracker finds it, refused when several sites do. Before any tracker
// is asked, the mirrors answer (Tracker.TicketsOfKey): a bare key whose
// project two sites' mirrors hold is refused as two sites' tickets, and a
// key one site's mirror holds the issue of resolves there, both with no
// request; a key one site's mirror holds the project of is looked up on
// that site first, and on the others when that site does not have it.
// The keys of one call that the mirrors do not answer are looked up
// together: one request per connection and round (resolveKeys).

// keyQuery is one key of a call as resolveKeys works it out: the key
// asked, the ticket once found, the connections each round asks, the hits
// so far, and the first refusal a connection gave.
type keyQuery struct {
	bare   string
	t      ticket
	done   bool
	rounds [][]trackerapp.TicketSource
	asked  []trackerapp.TicketSource
	hits   []ticket
	failed error
}

// keyParam is the argument a call names its keys with, as a refusal of
// one says it.
type keyParam struct{ which, where, naming string }

var (
	// keyArg is ticket_read's key.
	keyArg = keyParam{which: "key is", where: "with key", naming: "key names"}
	// keysArg is ticket_update's keys.
	keysArg = keyParam{which: "keys holds a key", where: "in keys", naming: "keys holds a key naming"}
)

// resolve is the ticket key names for thread threadID, and the sources on
// (resolveKeys).
func (s *Server) resolve(ctx context.Context, threadID, key string, param keyParam) (ticket, []trackerapp.TicketSource, error) {
	ts, sources, err := s.resolveKeys(ctx, threadID, []string{key}, param)
	if err != nil {
		return ticket{}, nil, err
	}
	return ts[0], sources, nil
}

// resolveKeys is the ticket each of keys names for thread threadID, in
// order, and the sources on. The first key that cannot be resolved fails
// the call.
func (s *Server) resolveKeys(ctx context.Context, threadID string, keys []string, param keyParam) ([]ticket, []trackerapp.TicketSource, error) {
	var link trackerapp.LinkedTicket
	linked, linkRead := false, false
	sources, err := s.sources()
	if err != nil {
		return nil, nil, err
	}
	queries := make([]*keyQuery, len(keys))
	for i, key := range keys {
		key = strings.TrimSpace(key)
		if len([]rune(key)) > maxKeyRunes {
			return nil, nil, invalidf("%s longer than any ticket key.", param.which)
		}
		site, bare, qualified := strings.Cut(key, "/")
		if !qualified {
			site, bare = "", key
		}
		site, bare = strings.TrimSpace(site), strings.ToUpper(strings.TrimSpace(bare))
		if !linkRead {
			var lerr error
			link, linked, lerr = s.t.LinkedTicket(threadID)
			var refused *trackerapp.LinkRefusal
			switch {
			case key == "" && lerr != nil:
				return nil, nil, lerr
			case errors.As(lerr, &refused):
				// The thread's ticket cannot be read here, which says nothing
				// of the ticket key names: the sources look for it.
				linked = false
			case lerr != nil:
				return nil, nil, lerr
			}
			linkRead = true
		}
		q := &keyQuery{bare: bare}
		queries[i] = q
		switch {
		case key == "" && !linked:
			return nil, nil, errorsx.Public(CodeNoTicket, "This thread is not linked to a ticket; name one "+param.where+".", nil)
		case key == "" || linked && strings.EqualFold(link.Key, bare) && (!qualified || strings.EqualFold(link.Site, site)):
			q.t = ticket{connection: link.Connection, site: link.Site, issue: link.Issue, key: link.Key, linked: true}
			q.done = true
			continue
		case bare == "":
			return nil, nil, invalidf("%s a site and no ticket; write it as site/KEY.", param.naming)
		}
		asked := sources
		if qualified {
			asked = slices.DeleteFunc(slices.Clone(sources), func(src trackerapp.TicketSource) bool { return !strings.EqualFold(src.Site, site) })
			if len(asked) == 0 {
				return nil, nil, errorsx.Public(CodeNotFound, fmt.Sprintf(
					"No connection on this computer reaches %s; the sites on are %s.", site, siteList(sources)), nil)
			}
		}
		held, err := s.t.TicketsOfKey(bare)
		if err != nil {
			return nil, nil, err
		}
		var mirrored []ticket
		for _, h := range held {
			if j := slices.IndexFunc(asked, func(src trackerapp.TicketSource) bool { return src.ID == h.Connection }); j >= 0 {
				mirrored = append(mirrored, ticket{connection: h.Connection, site: asked[j].Site, issue: h.Issue, key: bare})
			}
		}
		switch {
		case len(mirrored) > 1:
			return nil, nil, ambiguous(sources, bare, mirrored)
		case len(mirrored) == 1 && mirrored[0].issue != "":
			q.t, q.done = mirrored[0], true
		case len(mirrored) == 1:
			// The project's site first; the others when it does not have it.
			hinted := mirrored[0].site
			first := slices.DeleteFunc(slices.Clone(asked), func(src trackerapp.TicketSource) bool { return src.Site != hinted })
			rest := slices.DeleteFunc(slices.Clone(asked), func(src trackerapp.TicketSource) bool { return src.Site == hinted })
			q.rounds = [][]trackerapp.TicketSource{first, rest}
		default:
			q.rounds = [][]trackerapp.TicketSource{asked}
		}
	}
	if err := s.lookUp(ctx, sources, queries); err != nil {
		return nil, nil, err
	}
	out := make([]ticket, len(queries))
	for i, q := range queries {
		switch {
		case q.done:
			out[i] = q.t
		case len(q.hits) == 1:
			out[i] = q.hits[0]
		case len(q.hits) > 1:
			return nil, nil, ambiguous(sources, q.bare, q.hits)
		case q.failed != nil:
			// A source that could not answer may hold the key.
			return nil, nil, q.failed
		default:
			return nil, nil, errorsx.Public(CodeNotFound, fmt.Sprintf("No ticket %s on %s.", q.bare, siteList(q.asked)), nil)
		}
	}
	return out, sources, nil
}

// lookUp asks the trackers for the keys of queries the mirrors did not
// answer, round by round: in each round every connection, in order, is
// asked once for every key whose round names it and whose site has not
// found it, so two accounts on one site reaching a key reach one ticket.
// A key found in a round is not asked in the next. A refusal the model
// can read leaves the other sources asked, as ticket_find does; anything
// else fails the call.
func (s *Server) lookUp(ctx context.Context, sources []trackerapp.TicketSource, queries []*keyQuery) error {
	for round := 0; ; round++ {
		any := false
		for _, src := range sources {
			var asking []*keyQuery
			var keys []string
			for _, q := range queries {
				if q.done || round >= len(q.rounds) || round > 0 && len(q.hits) > 0 ||
					!slices.ContainsFunc(q.rounds[round], func(r trackerapp.TicketSource) bool { return r.ID == src.ID }) ||
					slices.ContainsFunc(q.hits, func(h ticket) bool { return h.site == src.Site }) {
					continue
				}
				if !slices.ContainsFunc(q.asked, func(a trackerapp.TicketSource) bool { return a.Site == src.Site }) {
					q.asked = append(q.asked, src)
				}
				asking = append(asking, q)
				if !slices.Contains(keys, q.bare) {
					keys = append(keys, q.bare)
				}
			}
			if len(keys) == 0 {
				continue
			}
			any = true
			found, err := s.t.Lookup(ctx, src.ID, keys)
			if err != nil {
				if _, _, public := errorsx.PublicDetails(err); !public || ctx.Err() != nil {
					return onSite(src.Site, err)
				}
				for _, q := range asking {
					if q.failed == nil {
						q.failed = onSite(src.Site, err)
					}
				}
				continue
			}
			for _, q := range asking {
				i := slices.IndexFunc(found.Found, func(m trackerapp.KeyMatchView) bool { return strings.EqualFold(m.Key, q.bare) })
				if i >= 0 {
					c := found.Found[i].Card
					q.hits = append(q.hits, ticket{connection: src.ID, site: src.Site, issue: c.ID, key: q.bare, title: c.Title})
				}
			}
		}
		if !any && round > 0 {
			return nil
		}
		if !slices.ContainsFunc(queries, func(q *keyQuery) bool { return !q.done && round+1 < len(q.rounds) }) {
			return nil
		}
	}
}

// ambiguous refuses bare, a key that hits name on several sites.
func ambiguous(sources []trackerapp.TicketSource, bare string, hits []ticket) error {
	sites, keys := make([]string, len(hits)), make([]string, len(hits))
	for i, h := range hits {
		sites[i], keys[i] = h.site, qualifier(sources, h.site)(h.key)
	}
	return errorsx.Public(CodeAmbiguous, fmt.Sprintf("%s is on %s; name one: %s.", bare, sayList(sites, "and"), sayList(keys, "or")), nil)
}

package trackertools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

// ticket_find searches every ticket source in the order the connections
// were made, one source after another: a page takes what the source at
// its start has, up to pageSize, and goes on to the next source only when
// that one has no more. So one answer asks at most one search of each
// source, and the model reads one list with one next.

type findArgs struct {
	Text string `json:"text"`
	Next string `json:"next"`
}

// findCursor is where a search goes on: the source, its tracker page
// token (empty for its first page), and the search's text as a digest, so
// a next is refused for a search of other words.
type findCursor struct {
	Connection string `json:"c"`
	Page       string `json:"p,omitempty"`
	Text       string `json:"t"`
}

// foundTicket is one row of an answer.
type foundTicket struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Status   string `json:"status,omitempty"`
	Assignee string `json:"assignee,omitempty"`
}

// unsearched is a source a page skipped because its search failed.
type unsearched struct {
	Site  string `json:"site"`
	Error string `json:"error"`
}

type findAnswer struct {
	Tickets    []foundTicket `json:"tickets"`
	Next       string        `json:"next,omitempty"`
	Unsearched []unsearched  `json:"unsearched,omitempty"`
}

func textDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}

func (s *Server) find(ctx context.Context, raw json.RawMessage) (any, error) {
	var args findArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(args.Text)
	if text == "" {
		return nil, invalidf("text is required.")
	}
	sources, err := s.sources()
	if err != nil {
		return nil, err
	}
	digest := textDigest(text)
	start, page := 0, ""
	if args.Next != "" {
		var at findCursor
		if err := decodeCursor("next", args.Next, &at); err != nil {
			return nil, err
		}
		if at.Text != digest {
			return nil, invalidf("next is from a search for other text; leave it out to search for this text.")
		}
		start = slices.IndexFunc(sources, func(src trackerapp.TicketSource) bool { return src.ID == at.Connection })
		if start < 0 {
			return nil, invalidf("next continues on a connection that is no longer on; leave it out to search again.")
		}
		page = at.Page
	}
	out := findAnswer{Tickets: []foundTicket{}}
	var failed error
	asked := 0
	for i := start; i < len(sources); i++ {
		src := sources[i]
		asked++
		got, err := s.t.Search(ctx, src.ID, trackerapp.SearchRequest{Text: text, Limit: pageSize - len(out.Tickets), Page: page})
		page = ""
		if err != nil {
			// A refusal the model can read (a connection turned off, a
			// tracker that failed or was too slow) leaves the other sources
			// searched and is named in the answer; anything else, or the
			// call's own end, fails the call.
			_, message, public := errorsx.PublicDetails(err)
			if !public || ctx.Err() != nil {
				return nil, onSite(src.Site, err)
			}
			if failed == nil {
				failed = onSite(src.Site, err)
			}
			out.Unsearched = append(out.Unsearched, unsearched{Site: src.Site, Error: message})
			continue
		}
		for _, card := range got.Cards {
			row := foundTicket{Key: card.Key, Title: card.Title, Status: card.Status.Name}
			if card.Assignee != nil {
				row.Assignee = card.Assignee.Name
			}
			out.Tickets = append(out.Tickets, row)
		}
		switch {
		case got.Next != "":
			out.Next = encodeCursor(findCursor{Connection: src.ID, Page: got.Next, Text: digest})
		case len(out.Tickets) >= pageSize && i+1 < len(sources):
			out.Next = encodeCursor(findCursor{Connection: sources[i+1].ID, Text: digest})
		}
		if out.Next != "" {
			break
		}
	}
	if len(out.Unsearched) == asked {
		// Every source this page asked failed: that is the answer.
		return nil, failed
	}
	return out, nil
}

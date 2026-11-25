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
		key := qualifier(sources, src.Site)
		for _, card := range got.Cards {
			row := foundTicket{Key: key(card.Key), Title: card.Title, Status: card.Status.Name}
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
	return fitFound(out)
}

// fitFound is out within answerBudget: each row's title, status and
// assignee cut at refCut and each refusal at refusalCut, and within those
// the texts share what the keys, the sites and next leave, as a read's
// parts do. Every row and next stay whole.
func fitFound(out findAnswer) (findAnswer, error) {
	var texts []string
	var demands []int
	add := func(text string, most int) {
		texts = append(texts, text)
		demands = append(demands, min(most, jsonLen(text)+2))
	}
	for _, row := range out.Tickets {
		add(row.Title, refCut)
		add(row.Status, refCut)
		add(row.Assignee, refCut)
	}
	for _, u := range out.Unsearched {
		add(u.Error, refusalCut)
	}
	weights := make([]int, len(demands))
	for i := range weights {
		weights[i] = 1
	}
	build := func(pool int) (findAnswer, error) {
		shares := allot(demands, weights, pool)
		n := 0
		clip := func() string {
			shown, _ := textClip(texts[n], 0, shares[n])
			n++
			return shown
		}
		cut := findAnswer{Tickets: make([]foundTicket, len(out.Tickets)), Next: out.Next}
		for i, row := range out.Tickets {
			cut.Tickets[i] = foundTicket{Key: row.Key, Title: clip(), Status: clip(), Assignee: clip()}
		}
		for _, u := range out.Unsearched {
			cut.Unsearched = append(cut.Unsearched, unsearched{Site: u.Site, Error: clip()})
		}
		return cut, nil
	}
	bare, _ := build(0)
	return fit("a search", answerBudget-sizeOf(bare), build)
}

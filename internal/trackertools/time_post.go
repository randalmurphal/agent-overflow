package trackertools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// time_draft with post: the agent asks the person to log the draft of
// named days. What it keeps true: nothing is sent until the person
// approved, in the app, an approval that shows every entry the post
// names, each with its ticket, day, time and whole reason; the post names
// exactly those entries at the revisions shown, so an entry changed,
// removed or added since is not sent as something the person did not see
// (trackerapp.Service.PostTimeEntries names a changed or removed one as
// not posted); and the post is a day's approval, with its rules: the
// logged-since skip, an entry that may already be logged named and not
// sent, a question left open.

// postSpan is the most days one post names.
const postSpan = readSpan

// postArg is post's days, as a refusal of one says it.
const postArg = "post"

// timePostSite is one connection's part of a post: its source, the
// entries the approval showed, and how many of them it offered to log.
type timePostSite struct {
	src     trackerapp.TicketSource
	entries []trackerapp.TimeEntryView
	offered int
}

// timePostAnswer is what a post did, a part per connection it posted on.
type timePostAnswer struct {
	Posts []timePosted `json:"posts"`
}

// timePosted is one connection's post: its site when the session has more
// than one connection, how many entries it logged of those it named that
// have a ticket, and the sentence that says each one not logged and why,
// or the error that stopped it.
type timePosted struct {
	Site     string `json:"site,omitempty"`
	Posted   int    `json:"posted"`
	Of       int    `json:"of"`
	Sentence string `json:"sentence"`
}

// TimePostRow is what one post did, for the thread's timeline: the call's
// ID, which names its row, the days it named, the connections it posted
// on, and how many of the entries the approval offered to log were
// logged. Written once, after the post ran, and never rewritten.
type TimePostRow struct {
	ID          string   `json:"call"`
	Connections []string `json:"connections"`
	Days        []string `json:"days"`
	Posted      int      `json:"posted"`
	Of          int      `json:"of"`
}

// postDays is post's days, sorted, each once: at most postSpan of them.
func postDays(post []string) ([]string, error) {
	if len(post) > postSpan {
		return nil, invalidf("%s names at most %d days; post the rest in another call.", postArg, postSpan)
	}
	days := make([]string, 0, len(post))
	for _, d := range post {
		d = strings.TrimSpace(d)
		if _, err := time.Parse(time.DateOnly, d); err != nil {
			return nil, invalidf("%s takes days as YYYY-MM-DD, not %q.", postArg, d)
		}
		if slices.Contains(days, d) {
			return nil, invalidf("%s names %s twice.", postArg, d)
		}
		days = append(days, d)
	}
	slices.Sort(days)
	return days, nil
}

func (s *Server) timePost(ctx context.Context, caller Caller, post []string) (any, error) {
	days, err := postDays(post)
	if err != nil {
		return nil, err
	}
	// Admission: a thread that may not log time is refused before
	// anything is read.
	if _, err := s.h.WritePolicy(caller.Thread); err != nil {
		return nil, err
	}
	sources, err := s.sources()
	if err != nil {
		return nil, err
	}
	jira, err := timeSources(sources)
	if err != nil {
		return nil, err
	}
	var sites []timePostSite
	offered := 0
	for _, src := range jira {
		got, err := s.t.OpenTimeEntries(src.ID, days[0], days[len(days)-1])
		if err != nil {
			return nil, onSite(src.Site, err)
		}
		got = slices.DeleteFunc(got, func(e trackerapp.TimeEntryView) bool { return !slices.Contains(days, e.Day) })
		if len(got) == 0 {
			continue
		}
		if len(got) > tracker.MaxWorklogInputs {
			return nil, invalidf("%s holds %d entries not yet logged on %s, more than one post takes (%d); post fewer days.",
				trackerapp.DayRange(days), len(got), src.Site, tracker.MaxWorklogInputs)
		}
		site := timePostSite{src: src, entries: got}
		for _, e := range got {
			if e.Key != "" && e.State == "drafted" {
				site.offered++
			}
		}
		offered += site.offered
		sites = append(sites, site)
	}
	if offered == 0 {
		return nil, invalidf("%s has no drafted entry on a ticket to log; time_read shows the draft.", trackerapp.DayRange(days))
	}
	id := s.newID()
	approval := timeApproval(id, days, sources, sites)
	if sizeOf(approval) > maxApprovalBytes {
		return nil, invalidf("%s's entries hold more than %d KiB, more than an approval shows the person; post fewer days in each call.",
			trackerapp.DayRange(days), maxApprovalBytes>>10)
	}
	if err := s.h.Approve(ctx, caller.Thread, approval); err != nil {
		return nil, err
	}
	// The send: admitted by the policy as it is now, so a thread made
	// read-only, or whose project turned off changes, while the person
	// looked logs nothing.
	if _, err := s.h.WritePolicy(caller.Thread); err != nil {
		return nil, err
	}
	// What was approved is sent whole: the call's end no longer stops it.
	wctx := context.WithoutCancel(ctx)
	out := timePostAnswer{Posts: []timePosted{}}
	row := TimePostRow{ID: id, Days: days, Of: offered}
	var failed error
	for _, site := range sites {
		refs := make([]trackerapp.TimeEntryRefView, len(site.entries))
		for i, e := range site.entries {
			refs[i] = trackerapp.TimeEntryRefView{ID: e.ID, Revision: e.Revision}
		}
		part := timePosted{Of: site.offered}
		if len(sources) > 1 {
			part.Site = site.src.Site
		}
		got, err := s.t.PostTime(wctx, site.src.ID, trackerapp.PostTimeRequest{Entries: refs})
		if err != nil {
			err = onSite(site.src.Site, err)
			if failed == nil {
				failed = err
			}
			_, message, public := errorsx.PublicDetails(err)
			if !public {
				s.logf("tracker tools: the time post of %s on %s: %v", caller.Thread, site.src.Site, err)
				message = site.src.Site + ": the post failed; time_read shows what was logged"
			}
			part.Sentence = message
			out.Posts = append(out.Posts, part)
			continue
		}
		part.Posted, part.Sentence = got.Posted, got.Sentence
		row.Connections = append(row.Connections, site.src.ID)
		row.Posted += got.Posted
		out.Posts = append(out.Posts, part)
	}
	if len(row.Connections) == 0 {
		// No post ran to its answer: that is the answer, and there is no
		// post to put on the timeline.
		return nil, failed
	}
	s.h.Posted(caller.Thread, row)
	share := answerBudget / len(out.Posts)
	for i := range out.Posts {
		out.Posts[i].Sentence, _ = textClip(out.Posts[i].Sentence, 0, share-64)
	}
	return out, nil
}

// timeApproval is the approval of a post of days: titled with them, each
// entry the post names under its ticket's key (qualified by its site when
// sources hold more than one connection), and each question, which has no
// key, in Notes, every one in the words trackerapp.EntryLine gives it.
func timeApproval(id string, days []string, sources []trackerapp.TicketSource, sites []timePostSite) Approval {
	a := Approval{ID: id, Title: "Log time for " + trackerapp.DayRange(days), Tool: "time_draft", Tickets: []ApprovalTicket{}}
	for _, site := range sites {
		key := qualifier(sources, site.src.Site)
		for _, e := range site.entries {
			line := trackerapp.EntryLine(e)
			if e.Key == "" {
				a.Notes = append(a.Notes, line)
				continue
			}
			k := key(e.Key)
			i := slices.IndexFunc(a.Tickets, func(t ApprovalTicket) bool { return t.Key == k })
			if i < 0 {
				title, _ := textClip(e.Title, 0, refCut)
				a.Tickets = append(a.Tickets, ApprovalTicket{Key: k, Title: title})
				i = len(a.Tickets) - 1
			}
			a.Tickets[i].Changes = append(a.Tickets[i].Changes, ApprovalChange{Line: line})
		}
	}
	return a
}

// Title is row's title in the thread's timeline: "Logged 2 of 3 entries
// for Tue Nov 24".
func (row TimePostRow) Title() string {
	return fmt.Sprintf("Logged %d of %d entries for %s", row.Posted, row.Of, trackerapp.DayRange(row.Days))
}

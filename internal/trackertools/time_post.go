package trackertools

import (
	"cmp"
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

// timePostSite is one connection's part of a post: its source, how its
// keys are written, the entries the approval showed, and how many of them
// it offered to log.
type timePostSite struct {
	src     trackerapp.TicketSource
	key     func(string) string
	entries []trackerapp.TimeEntryView
	offered int
}

// timePostAnswer is what a post did with each entry it named, as the
// approval showed it: those logged, those not logged grouped by why, and
// the connections whose entries could not be read.
type timePostAnswer struct {
	Logged    []timeLine      `json:"logged"`
	NotLogged []timeNotLogged `json:"not_logged,omitempty"`
	Unread    []timeUnread    `json:"unread,omitempty"`
}

// timeLine is an entry as the approval showed it: its day, its ticket's
// key (none for a question) and its hours.
type timeLine struct {
	Day   string  `json:"day"`
	Key   string  `json:"key,omitempty"`
	Hours float64 `json:"hours"`
}

// timeNotLogged is the entries not logged for one reason.
type timeNotLogged struct {
	Why     string     `json:"why"`
	Entries []timeLine `json:"entries"`
}

// TimePostRow is what one post did, for the thread's timeline: the call's
// ID, which names its row, the days it named, the connections it posted
// on, how many of the entries the approval offered to log were logged,
// and how many of those may have been. Written once, after the post ran,
// and never rewritten.
type TimePostRow struct {
	ID          string   `json:"call"`
	Connections []string `json:"connections"`
	Days        []string `json:"days"`
	Posted      int      `json:"posted"`
	Of          int      `json:"of"`
	Uncertain   int      `json:"uncertain,omitempty"`
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
	var unread []timeUnread
	var failed error
	total, offered := 0, 0
	for _, src := range jira {
		if src.Account == "" {
			// Never verified: a draft needs the verified account, so the
			// connection holds none.
			continue
		}
		got, err := s.t.OpenTimeEntries(src.ID, days)
		if err != nil {
			// A refusal the model can read leaves the other connections
			// posted and is named in the answer and the approval, as
			// time_read names it; anything else fails the call.
			_, message, public := errorsx.PublicDetails(err)
			if !public || ctx.Err() != nil {
				return nil, onSite(src.Site, err)
			}
			if failed == nil {
				failed = onSite(src.Site, err)
			}
			message, _ = textClip(message, 0, refusalCut)
			unread = append(unread, timeUnread{Site: src.Site, Error: message})
			continue
		}
		if len(got) == 0 {
			continue
		}
		site := timePostSite{src: src, key: postKey(sources, jira, src), entries: got}
		for _, e := range got {
			if trackerapp.NotSentWhy(e) == "" {
				site.offered++
			}
		}
		total += len(got)
		offered += site.offered
		sites = append(sites, site)
	}
	switch {
	case len(sites) == 0 && failed != nil:
		return nil, failed
	case len(sites) == 0:
		return nil, invalidf("%s has no entry not yet logged; time_read shows the draft.", trackerapp.DayRange(days))
	case total > tracker.MaxWorklogInputs:
		return nil, invalidf("%s holds %d entries not yet logged, more than one post takes (%d); post fewer days.",
			trackerapp.DayRange(days), total, tracker.MaxWorklogInputs)
	case postAnswerMost(sites, unread) > answerBudget:
		return nil, invalidf("%s holds more entries not yet logged than one answer names; post fewer days in each call.", trackerapp.DayRange(days))
	}
	if offered == 0 {
		// Nothing the post would send: each entry's reason is the answer,
		// and there is nothing to ask.
		out := timePostAnswer{Logged: []timeLine{}, Unread: unread}
		for _, site := range sites {
			for _, e := range site.entries {
				out.NotLogged = notLogged(out.NotLogged, trackerapp.NotSentWhy(e), site.line(e))
			}
		}
		return fitPostAnswer(out)
	}
	id := s.newID()
	approval := timeApproval(id, days, sites, unread)
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
	out := timePostAnswer{Logged: []timeLine{}, Unread: unread}
	row := TimePostRow{ID: id, Days: days, Of: offered}
	failed = nil
	for _, site := range sites {
		refs := make([]trackerapp.TimeEntryRefView, len(site.entries))
		for i, e := range site.entries {
			refs[i] = trackerapp.TimeEntryRefView{ID: e.ID, Revision: e.Revision}
		}
		got, err := s.t.PostTime(wctx, site.src.ID, trackerapp.PostTimeRequest{Entries: refs})
		if err != nil {
			err = onSite(site.src.Site, err)
			s.logf("tracker tools: the time post of %s on %s: %v", caller.Thread, site.src.Site, err)
		}
		if len(got.Outcomes) == 0 {
			// Refused before anything was sent.
			if failed == nil {
				failed = err
			}
			_, message, public := errorsx.PublicDetails(err)
			if !public {
				message = site.src.Site + ": the post failed"
			}
			for _, e := range site.entries {
				why := trackerapp.NotSentWhy(e)
				if why == "" {
					why = "was not sent: " + message
				}
				out.NotLogged = notLogged(out.NotLogged, why, site.line(e))
			}
			continue
		}
		if len(got.Outcomes) != len(refs) {
			return nil, fmt.Errorf("trackertools: the post on %s answered %d outcomes for %d entries", site.src.Site, len(got.Outcomes), len(refs))
		}
		row.Connections = append(row.Connections, site.src.ID)
		for i, o := range got.Outcomes {
			e := site.entries[i]
			switch o.State {
			case trackerapp.OutcomePosted:
				row.Posted++
				out.Logged = append(out.Logged, site.line(e))
				continue
			case trackerapp.OutcomeUncertain:
				if trackerapp.NotSentWhy(e) == "" {
					row.Uncertain++
				}
			}
			out.NotLogged = notLogged(out.NotLogged, o.Why, site.line(e))
		}
	}
	if len(row.Connections) == 0 {
		// No post sent anything: that is the answer, and there is no post
		// to put on the timeline.
		return nil, failed
	}
	s.h.Posted(caller.Thread, row)
	return fitPostAnswer(out)
}

// line is entry e as the approval showed it.
func (site timePostSite) line(e trackerapp.TimeEntryView) timeLine {
	return timeLine{Day: e.Day, Key: site.key(e.Key), Hours: float64(e.Minutes) / 60}
}

// notLogged is groups with line added under why.
func notLogged(groups []timeNotLogged, why string, line timeLine) []timeNotLogged {
	if i := slices.IndexFunc(groups, func(g timeNotLogged) bool { return g.Why == why }); i >= 0 {
		groups[i].Entries = append(groups[i].Entries, line)
		return groups
	}
	return append(groups, timeNotLogged{Why: why, Entries: []timeLine{line}})
}

// postAnswerMost is the most bytes a post's answer of sites' entries can
// take, with each entry under a reason of its own cut to nothing: what
// fitPostAnswer cuts the reasons to fit.
func postAnswerMost(sites []timePostSite, unread []timeUnread) int {
	n := sizeOf(timePostAnswer{Logged: []timeLine{}, NotLogged: []timeNotLogged{}, Unread: unread})
	group := sizeOf(timeNotLogged{Entries: []timeLine{}}) + 1
	for _, site := range sites {
		for _, e := range site.entries {
			n += group + sizeOf(site.line(e)) + 1
		}
	}
	return n
}

// fitPostAnswer is out within answerBudget: each reason cut at
// refusalCut, and within that the reasons share what the entries leave.
// Every entry stays (postAnswerMost).
func fitPostAnswer(out timePostAnswer) (timePostAnswer, error) {
	demands := make([]int, len(out.NotLogged))
	weights := make([]int, len(out.NotLogged))
	for i, g := range out.NotLogged {
		demands[i], weights[i] = min(refusalCut, jsonLen(g.Why)+2), 1
	}
	build := func(pool int) (timePostAnswer, error) {
		shares := allot(demands, weights, pool)
		cut := out
		cut.NotLogged = make([]timeNotLogged, len(out.NotLogged))
		for i, g := range out.NotLogged {
			g.Why, _ = textClip(g.Why, 0, shares[i])
			cut.NotLogged[i] = g
		}
		return cut, nil
	}
	bare, _ := build(0)
	return fit("the time post", answerBudget-sizeOf(bare), build)
}

// postKey writes a key of src's as a post's approval and answer show it:
// qualified by its site when sources hold more than one connection, and,
// when another of the Jira connections jira is of src's site, by the name
// the person gave src in the Issues settings, or its account email when
// they gave none.
func postKey(sources, jira []trackerapp.TicketSource, src trackerapp.TicketSource) func(string) string {
	key := qualifier(sources, src.Site)
	if !slices.ContainsFunc(jira, func(o trackerapp.TicketSource) bool { return o.ID != src.ID && o.Site == src.Site }) {
		return key
	}
	name, _ := textClip(cmp.Or(src.Label, src.Email), 0, refCut)
	return func(k string) string {
		if k == "" {
			return ""
		}
		return key(k) + " (" + name + ")"
	}
}

// timeApproval is the approval of a post of days: titled with them, each
// entry the post names under its ticket's key as postKey writes it, and
// each question, which has no key, and each connection that could not be
// read in Notes, every entry in the words trackerapp.EntryLine gives it.
func timeApproval(id string, days []string, sites []timePostSite, unread []timeUnread) Approval {
	a := Approval{ID: id, Title: "Log time for " + trackerapp.DayRange(days), Tool: "time_draft", Tickets: []ApprovalTicket{}}
	for _, site := range sites {
		for _, e := range site.entries {
			line := trackerapp.EntryLine(e)
			if e.Key == "" {
				a.Notes = append(a.Notes, line)
				continue
			}
			k := site.key(e.Key)
			i := slices.IndexFunc(a.Tickets, func(t ApprovalTicket) bool { return t.Key == k })
			if i < 0 {
				title, _ := textClip(e.Title, 0, refCut)
				a.Tickets = append(a.Tickets, ApprovalTicket{Key: k, Title: title})
				i = len(a.Tickets) - 1
			}
			a.Tickets[i].Changes = append(a.Tickets[i].Changes, ApprovalChange{Line: line})
		}
	}
	for _, u := range unread {
		a.Notes = append(a.Notes, fmt.Sprintf("%s could not be read: %s; its time is not in this post.", u.Site, strings.TrimSuffix(u.Error, ".")))
	}
	return a
}

// Title is row's title in the thread's timeline: "Logged 2 of 3 entries
// for Tue Nov 24", and "; 1 may have been logged" when the post lost the
// answer of some.
func (row TimePostRow) Title() string {
	title := fmt.Sprintf("Logged %d of %d entries for %s", row.Posted, row.Of, trackerapp.DayRange(row.Days))
	if row.Uncertain > 0 {
		title += fmt.Sprintf("; %d may have been logged", row.Uncertain)
	}
	return title
}

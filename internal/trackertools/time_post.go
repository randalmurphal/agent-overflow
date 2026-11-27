package trackertools

import (
	"cmp"
	"context"
	"errors"
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
// keys are written, its name when another connection of its site posts
// too (postAccount), the entries the approval showed, and how many of
// them it offered to log.
type timePostSite struct {
	src     trackerapp.TicketSource
	key     func(string) string
	account string
	entries []trackerapp.TimeEntryView
	offered int
}

// timePostAnswer is what a post did with each entry it named, as the
// approval showed it: those logged, those not logged and those that may
// be logged, each grouped by why, the connections whose entries could not
// be read, and what a connection's post left unrecorded or unread here.
type timePostAnswer struct {
	Logged      []timeLine      `json:"logged"`
	NotLogged   []timeNotLogged `json:"not_logged,omitempty"`
	MayBeLogged []timeNotLogged `json:"may_be_logged,omitempty"`
	Unread      []timeUnread    `json:"unread,omitempty"`
	Warnings    []timeWarning   `json:"warnings,omitempty"`
}

// timeLine is an entry as the approval showed it: its day, its ticket's
// key (none for a question), its connection's name (postAccount) and its
// hours.
type timeLine struct {
	Day     string  `json:"day"`
	Key     string  `json:"key,omitempty"`
	Account string  `json:"account,omitempty"`
	Hours   float64 `json:"hours"`
}

// timeWarning is what a connection's post did that its entries' lists do
// not say: a day whose logged time this computer could not read back, or
// a logged entry it could not record.
type timeWarning struct {
	Site    string `json:"site"`
	Account string `json:"account,omitempty"`
	Warning string `json:"warning"`
}

// timeNotLogged is the entries not logged, or that may be, for one reason.
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
	// codes are unread's refusal codes, in its order.
	var codes []string
	// read is whether a connection was read: it holds no entry when no
	// site does.
	read := false
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
			code, message, public := errorsx.PublicDetails(err)
			if !public || ctx.Err() != nil {
				return nil, onSite(src.Site, err)
			}
			codes = append(codes, code)
			message, _ = textClip(message, 0, refusalCut)
			unread = append(unread, timeUnread{Site: src.Site, Account: postAccount(jira, src), Error: message})
			continue
		}
		read = true
		if len(got) == 0 {
			continue
		}
		site := timePostSite{src: src, key: qualifier(sources, src.Site), account: postAccount(jira, src), entries: got}
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
	case len(sites) == 0 && len(unread) > 0:
		return nil, unreadRefusal(days, unread, codes, read)
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
				out.add(trackerapp.NotSentOutcome(e), site.line(e))
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
			_, message, public := errorsx.PublicDetails(err)
			if !public {
				message = site.src.Site + ": the post failed"
			}
			for _, e := range site.entries {
				o := trackerapp.NotSentOutcome(e)
				if o.Why == "" {
					o.Why = "was not sent: " + message
				}
				out.add(o, site.line(e))
			}
			continue
		}
		if len(got.Outcomes) != len(refs) {
			return nil, fmt.Errorf("trackertools: the post on %s answered %d outcomes for %d entries", site.src.Site, len(got.Outcomes), len(refs))
		}
		row.Connections = append(row.Connections, site.src.ID)
		for i, o := range got.Outcomes {
			e := site.entries[i]
			switch {
			case o.State == trackerapp.OutcomePosted:
				row.Posted++
			case o.State == trackerapp.OutcomeUncertain && trackerapp.NotSentWhy(e) == "":
				row.Uncertain++
			}
			out.add(o, site.line(e))
		}
		var warn []string
		if missed := trackerapp.ReadBackSentence(got.Stale); missed != "" {
			warn = append(warn, missed)
		}
		if errors.Is(err, trackerapp.ErrPostUnrecorded) {
			// The send happened, and this computer could not record its
			// outcomes: the entries stay sent while the app runs, and read
			// as may already be logged after it restarts. The cause is the
			// person's to fix (a full disk, a locked database), so it is
			// said. An error after the outcomes were stored (the read of
			// the entries after them) changes nothing the answer says and
			// is only logged.
			warn = append(warn, fmt.Sprintf("The time listed as logged is logged, but this computer could not record it (%s); "+
				"the Time screen shows these entries as may already be logged after the app restarts.", strings.TrimSuffix(err.Error(), ".")))
		}
		if len(warn) > 0 {
			out.Warnings = append(out.Warnings, timeWarning{Site: site.src.Site, Account: site.account, Warning: strings.Join(warn, " ")})
		}
	}
	if len(row.Connections) > 0 {
		// A post that sent nothing has nothing to put on the timeline; its
		// answer says why.
		s.h.Posted(caller.Thread, row)
	}
	return fitPostAnswer(out)
}

// line is entry e as the approval showed it.
func (site timePostSite) line(e trackerapp.TimeEntryView) timeLine {
	return timeLine{Day: e.Day, Key: site.key(e.Key), Account: site.account, Hours: float64(e.Minutes) / 60}
}

// add puts line, an entry the post named, in out by its outcome o: logged,
// may be logged or not logged, each under o's why.
func (out *timePostAnswer) add(o trackerapp.PostOutcome, line timeLine) {
	under := func(groups []timeNotLogged) []timeNotLogged {
		if i := slices.IndexFunc(groups, func(g timeNotLogged) bool { return g.Why == o.Why }); i >= 0 {
			groups[i].Entries = append(groups[i].Entries, line)
			return groups
		}
		return append(groups, timeNotLogged{Why: o.Why, Entries: []timeLine{line}})
	}
	switch o.State {
	case trackerapp.OutcomePosted:
		out.Logged = append(out.Logged, line)
	case trackerapp.OutcomeUncertain:
		out.MayBeLogged = under(out.MayBeLogged)
	default:
		out.NotLogged = under(out.NotLogged)
	}
}

// postAnswerMost is the most bytes a post's answer of sites' entries can
// take: the answer with each entry under a reason of its own, in both
// lists of reasons, and each connection with a warning, every text cut to
// nothing, which is what fitPostAnswer cuts the texts to fit.
func postAnswerMost(sites []timePostSite, unread []timeUnread) int {
	most := timePostAnswer{Logged: []timeLine{}, Unread: unread}
	for _, site := range sites {
		most.Warnings = append(most.Warnings, timeWarning{Site: site.src.Site, Account: site.account})
		for _, e := range site.entries {
			group := []timeNotLogged{{Entries: []timeLine{site.line(e)}}}
			if most.MayBeLogged == nil {
				most.MayBeLogged = group
			} else {
				most.NotLogged = append(most.NotLogged, group...)
			}
		}
	}
	return sizeOf(most)
}

// fitPostAnswer is out within answerBudget: each reason and warning cut
// at refusalCut, and within that they share what the entries leave.
// Every entry stays (postAnswerMost).
func fitPostAnswer(out timePostAnswer) (timePostAnswer, error) {
	var demands, weights []int
	add := func(text string) {
		demands = append(demands, min(refusalCut, jsonLen(text)+2))
		weights = append(weights, 1)
	}
	for _, g := range out.NotLogged {
		add(g.Why)
	}
	for _, g := range out.MayBeLogged {
		add(g.Why)
	}
	for _, w := range out.Warnings {
		add(w.Warning)
	}
	build := func(pool int) (timePostAnswer, error) {
		shares := allot(demands, weights, pool)
		clip := func(text string) string {
			text, _ = textClip(text, 0, shares[0])
			shares = shares[1:]
			return text
		}
		cut := out
		cut.NotLogged = slices.Clone(out.NotLogged)
		for i := range cut.NotLogged {
			cut.NotLogged[i].Why = clip(cut.NotLogged[i].Why)
		}
		cut.MayBeLogged = slices.Clone(out.MayBeLogged)
		for i := range cut.MayBeLogged {
			cut.MayBeLogged[i].Why = clip(cut.MayBeLogged[i].Why)
		}
		cut.Warnings = slices.Clone(out.Warnings)
		for i := range cut.Warnings {
			cut.Warnings[i].Warning = clip(cut.Warnings[i].Warning)
		}
		return cut, nil
	}
	bare, _ := build(0)
	return fit("the time post", answerBudget-sizeOf(bare), build)
}

// postAccount names src in a post's approval and answer when another of
// the Jira connections jira is of src's site, and is "" when none is: by
// the name the person gave src in the Issues settings, or its account
// email when they gave none. When two of the site's connections would go
// by one name, each of them goes by "label · email", or its email when it
// has no label: emails are unique on a site (the connections' identity
// index) and hold no space, so no two connections of a site share a name.
func postAccount(jira []trackerapp.TicketSource, src trackerapp.TicketSource) string {
	var names []string
	for _, o := range jira {
		if o.Site == src.Site {
			names = append(names, cmp.Or(o.Label, o.Email))
		}
	}
	if len(names) < 2 {
		return ""
	}
	slices.Sort(names)
	if len(slices.Compact(slices.Clone(names))) == len(names) {
		return cmp.Or(src.Label, src.Email)
	}
	if src.Label == "" {
		return src.Email
	}
	return src.Label + " · " + src.Email
}

// timeApproval is the approval of a post of days: titled with them, each
// entry the post names under its ticket of its connection, and each
// question, which has no key, and each connection that could not be read
// in Notes, every entry in the words trackerapp.EntryLine gives it.
func timeApproval(id string, days []string, sites []timePostSite, unread []timeUnread) Approval {
	a := Approval{ID: id, Title: "Log time for " + trackerapp.DayRange(days), Tool: "time_draft", Tickets: []ApprovalTicket{}}
	for _, site := range sites {
		for _, e := range site.entries {
			line := trackerapp.EntryLine(e)
			if e.Key == "" {
				if site.account != "" {
					line += " (" + site.account + ")"
				}
				a.Notes = append(a.Notes, line)
				continue
			}
			k := site.key(e.Key)
			i := slices.IndexFunc(a.Tickets, func(t ApprovalTicket) bool { return t.Key == k && t.Account == site.account })
			if i < 0 {
				title, _ := textClip(e.Title, 0, refCut)
				a.Tickets = append(a.Tickets, ApprovalTicket{Key: k, Account: site.account, Title: title})
				i = len(a.Tickets) - 1
			}
			a.Tickets[i].Changes = append(a.Tickets[i].Changes, ApprovalChange{Line: line})
		}
	}
	for _, u := range unread {
		a.Notes = append(a.Notes, u.sentence()+"; its time is not in this post.")
	}
	return a
}

// sentence says that u could not be read, and why: "example.atlassian.net
// (Work) could not be read: …", as the approval's notes and a refusal say
// it.
func (u timeUnread) sentence() string {
	name := u.Site
	if u.Account != "" {
		name += " (" + u.Account + ")"
	}
	return fmt.Sprintf("%s could not be read: %s", name, strings.TrimSuffix(u.Error, "."))
}

// unreadRefusal is a post's refusal when no connection it could read
// holds an entry of days: each connection that could not be read and why,
// with its refusal's code when they share one, and that the others, when
// any was read, hold none.
func unreadRefusal(days []string, unread []timeUnread, codes []string, read bool) error {
	lines := make([]string, len(unread))
	for i, u := range unread {
		lines[i] = u.sentence()
	}
	code := codes[0]
	if slices.ContainsFunc(codes, func(c string) bool { return c != code }) {
		code = CodeFailed
	}
	text := fmt.Sprintf("Nothing was posted for %s: %s.", trackerapp.DayRange(days), strings.Join(lines, "; "))
	if read {
		text += " The other connections hold no entry not yet logged for those days."
	}
	return errorsx.Public(code, text, nil)
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

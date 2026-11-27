package trackertools

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

// time_read and time_draft: the user's time on this computer's Jira
// connections, each the time of its verified account. time_read answers
// the days' logged time and the time draft (trackerapp.Service.ReadTime);
// time_draft replaces the agents' entries of each day it names
// (trackerapp.Service.DraftTime). Neither sends time: nothing reaches a
// posting path, and a draft is logged only once the person approves it.
// A session is given them only when a Jira connection was on as it
// started (Server.Tools).

// readSpan is the most days one time_read answers.
const readSpan = 7

// jiraKind is a Jira connection's kind (store.TrackerKindJira).
const jiraKind = "jira"

type timeReadArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// loggedTime is a worklog of a day.
type loggedTime struct {
	Key         string  `json:"key"`
	Title       string  `json:"title,omitempty"`
	Hours       float64 `json:"hours"`
	Description string  `json:"description,omitempty"`
}

// draftedTime is an entry of the draft on a ticket: by agent or you,
// the person; State is drafted, sent or unknown.
type draftedTime struct {
	ID     int64   `json:"id"`
	Key    string  `json:"key"`
	Hours  float64 `json:"hours"`
	Reason string  `json:"reason,omitempty"`
	By     string  `json:"by,omitempty"`
	State  string  `json:"state,omitempty"`
}

// timeQuestion is an open question of the draft: hours whose ticket the
// person picks, what the time was and the keys it offers.
type timeQuestion struct {
	ID     int64    `json:"id"`
	Hours  float64  `json:"hours"`
	Reason string   `json:"reason"`
	Offer  []string `json:"offer,omitempty"`
}

// timeDay is a day of time_read: the time logged and drafted, and what
// this computer measured of it (readActivity): Active, the minutes with
// the person's input in the app, and Phone, those in the compact layout;
// the tickets the focused threads are linked to, each with its minutes,
// or none for a ticket only a turn that day touched; and the commits.
type timeDay struct {
	Day         string         `json:"day"`
	Logged      []loggedTime   `json:"logged"`
	Drafted     []draftedTime  `json:"drafted,omitempty"`
	Questions   []timeQuestion `json:"questions,omitempty"`
	Active      int            `json:"active,omitempty"`
	Phone       int            `json:"phone,omitempty"`
	Tickets     []activeTicket `json:"tickets,omitempty"`
	Commits     []commitLine   `json:"commits,omitempty"`
	CommitsMore int            `json:"commits_more,omitempty"`
}

// activeTicket is a ticket a day's threads touched and the minutes of
// input on them.
type activeTicket struct {
	Key     string `json:"key"`
	Minutes int    `json:"minutes,omitempty"`
}

// commitLine is a commit of the day: its local time (15:04) and subject.
type commitLine struct {
	At      string `json:"at"`
	Subject string `json:"subject"`
}

// commitsShown is the most commits a day of time_read names; commits_more
// counts the rest.
const commitsShown = 30

// timeUnread is a connection whose time could not be read from its
// tracker: the answer holds what this computer read before, and Days are
// those it never read.
type timeUnread struct {
	Site  string   `json:"site"`
	Error string   `json:"error"`
	Days  []string `json:"days,omitempty"`
}

// timeReadAnswer is time_read's answer; ActivityGap says since when the
// app has not recorded the person's activity, while it cannot.
type timeReadAnswer struct {
	Days        []timeDay    `json:"days"`
	Unread      []timeUnread `json:"unread,omitempty"`
	ActivityGap string       `json:"activity_gap,omitempty"`
}

// timeSources are the Jira connections among sources; none is the
// refusal that says how to get one.
func timeSources(sources []trackerapp.TicketSource) ([]trackerapp.TicketSource, error) {
	out := slices.DeleteFunc(slices.Clone(sources), func(src trackerapp.TicketSource) bool { return src.Kind != jiraKind })
	if len(out) == 0 {
		return nil, errorsx.Public(CodeNoConnection,
			"This computer has no Jira connection turned on, and time is kept on Jira. The user adds or turns one on in the Issues settings.", nil)
	}
	return out, nil
}

// readRange is time_read's days: from to to, this week (Monday to
// Sunday) when neither is given, and 7 days from the one given.
func (s *Server) readRange(args timeReadArgs) (string, string, error) {
	parse := func(day string) (time.Time, bool) {
		t, err := time.Parse(time.DateOnly, strings.TrimSpace(day))
		return t, err == nil
	}
	from, okFrom := parse(args.From)
	to, okTo := parse(args.To)
	switch {
	case args.From == "" && args.To == "":
		y, m, d := s.now().Date()
		today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		from = today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
		to = from.AddDate(0, 0, readSpan-1)
	case args.From != "" && !okFrom, args.To != "" && !okTo:
		return "", "", invalidf("from and to take YYYY-MM-DD.")
	case args.To == "":
		to = from.AddDate(0, 0, readSpan-1)
	case args.From == "":
		from = to.AddDate(0, 0, -(readSpan - 1))
	case to.Before(from):
		return "", "", invalidf("to is before from.")
	case to.Sub(from) >= readSpan*24*time.Hour:
		return "", "", invalidf("time_read covers at most %d days; ask again for the rest.", readSpan)
	}
	return from.Format(time.DateOnly), to.Format(time.DateOnly), nil
}

func (s *Server) timeRead(ctx context.Context, _ Caller, raw json.RawMessage) (any, error) {
	var args timeReadArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	from, to, err := s.readRange(args)
	if err != nil {
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
	first, _ := time.Parse(time.DateOnly, from)
	last, _ := time.Parse(time.DateOnly, to)
	out := timeReadAnswer{}
	byDay := map[string]*timeDay{}
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		out.Days = append(out.Days, timeDay{Day: d.Format(time.DateOnly), Logged: []loggedTime{}})
	}
	for i := range out.Days {
		byDay[out.Days[i].Day] = &out.Days[i]
	}
	var failed error
	failures := 0
	for _, src := range jira {
		got, err := s.t.ReadTime(ctx, src.ID, from, to)
		if err != nil {
			// A refusal the model can read (a connection turned off, one
			// with no verified account) leaves the other connections read
			// and is named in the answer; anything else, or the call's own
			// end, fails the call.
			_, message, public := errorsx.PublicDetails(err)
			if !public || ctx.Err() != nil {
				return nil, onSite(src.Site, err)
			}
			if failed == nil {
				failed = onSite(src.Site, err)
			}
			failures++
			out.Unread = append(out.Unread, timeUnread{Site: src.Site, Error: message})
			continue
		}
		switch {
		case got.Failure != nil:
			out.Unread = append(out.Unread, timeUnread{Site: src.Site, Error: got.Failure.Message, Days: got.Unread})
		case got.More:
			out.Unread = append(out.Unread, timeUnread{Site: src.Site, Error: "these days hold more worklogs than one answer shows; ask for fewer days"})
		}
		key := qualifier(sources, src.Site)
		for _, w := range got.Worklogs {
			if day := byDay[w.Day]; day != nil {
				day.Logged = append(day.Logged, loggedTime{Key: key(w.Key), Title: w.Title, Hours: float64(w.SpentSeconds) / 3600,
					Description: strings.TrimSpace(w.Description)})
			}
		}
		for _, e := range got.Drafted {
			day := byDay[e.Day]
			switch {
			case day == nil || e.State == "posted" || e.State == "left_out":
				// A posted entry is among the logged worklogs.
			case e.Key == "":
				q := timeQuestion{ID: e.ID, Hours: float64(e.Minutes) / 60, Reason: e.Reason}
				for _, o := range e.Offers {
					q.Offer = append(q.Offer, key(o.Key))
				}
				day.Questions = append(day.Questions, q)
			default:
				day.Drafted = append(day.Drafted, draftedTime{ID: e.ID, Key: key(e.Key), Hours: float64(e.Minutes) / 60, Reason: e.Reason,
					By: byName(e.By), State: e.State})
			}
		}
	}
	if failures == len(jira) {
		// Every connection failed: that is the answer.
		return nil, failed
	}
	if err := s.readActivity(ctx, sources, from, to, &out); err != nil {
		return nil, err
	}
	return fitTime(out)
}

// readActivity adds to out's days what this computer measured of them
// (Host.Activity): the minutes with input, the phone's when there are
// any, the minutes on each Jira ticket the focused threads are linked to
// (a thread's link as it is now) and the tickets of the threads that
// started a turn, and the day's commits, at most commitsShown; and the
// recording gap while there is one. Hours are never a thread's run time.
func (s *Server) readActivity(ctx context.Context, sources []trackerapp.TicketSource, from, to string, out *timeReadAnswer) error {
	act, err := s.h.Activity(ctx, from, to)
	if err != nil {
		return fmt.Errorf("trackertools: the activity of %s to %s: %w", from, to, err)
	}
	if act.Gap != 0 {
		out.ActivityGap = "since " + time.UnixMilli(act.Gap).Format("2006-01-02 15:04")
	}
	var threads []string
	for _, d := range act.Days {
		for id := range d.Threads {
			if id != "" {
				threads = append(threads, id)
			}
		}
		threads = append(threads, d.Ran...)
	}
	links := map[string]trackerapp.JiraTicket{}
	if len(threads) > 0 {
		if links, err = s.t.ThreadJiraTickets(threads); err != nil {
			return err
		}
	}
	minutes := func(d time.Duration) int { return int((d + 30*time.Second) / time.Minute) }
	for _, d := range act.Days {
		i := slices.IndexFunc(out.Days, func(day timeDay) bool { return day.Day == d.Day.Day })
		if i < 0 {
			continue
		}
		day := &out.Days[i]
		day.Active, day.Phone = minutes(d.Active), minutes(d.Phone)
		on := map[trackerapp.JiraTicket]time.Duration{}
		for id, spent := range d.Threads {
			if t, ok := links[id]; ok {
				on[t] += spent
			}
		}
		for _, id := range d.Ran {
			if t, ok := links[id]; ok {
				on[t] += 0
			}
		}
		for t, spent := range on {
			day.Tickets = append(day.Tickets, activeTicket{Key: qualifier(sources, t.Site)(t.Key), Minutes: minutes(spent)})
		}
		slices.SortFunc(day.Tickets, func(a, b activeTicket) int {
			return cmp.Or(cmp.Compare(b.Minutes, a.Minutes), strings.Compare(a.Key, b.Key))
		})
		for _, c := range d.Commits[:min(len(d.Commits), commitsShown)] {
			day.Commits = append(day.Commits, commitLine{At: c.At.Local().Format("15:04"), Subject: c.Subject})
		}
		day.CommitsMore = len(d.Commits) - len(day.Commits)
	}
	return nil
}

// byName is who last wrote an entry, as answers name them: agent, or you
// for the person.
func byName(by string) string {
	if by == "person" {
		return "you"
	}
	return by
}

// fitTime is out within answerBudget: each title cut at refCut and each
// description, reason and question at refusalCut, and within those the
// texts share what the rest leaves, as a search's rows do.
func fitTime(out timeReadAnswer) (timeReadAnswer, error) {
	var texts []string
	var demands []int
	add := func(text string, most int) {
		texts = append(texts, text)
		demands = append(demands, min(most, jsonLen(text)+2))
	}
	for _, d := range out.Days {
		for _, w := range d.Logged {
			add(w.Title, refCut)
			add(w.Description, refusalCut)
		}
		for _, e := range d.Drafted {
			add(e.Reason, refusalCut)
		}
		for _, q := range d.Questions {
			add(q.Reason, refusalCut)
		}
		for _, c := range d.Commits {
			add(c.Subject, refCut)
		}
	}
	weights := make([]int, len(demands))
	for i := range weights {
		weights[i] = 1
	}
	build := func(pool int) (timeReadAnswer, error) {
		shares := allot(demands, weights, pool)
		n := 0
		clip := func() string {
			shown, _ := textClip(texts[n], 0, shares[n])
			n++
			return shown
		}
		cut := timeReadAnswer{Days: make([]timeDay, len(out.Days)), Unread: out.Unread, ActivityGap: out.ActivityGap}
		for i, d := range out.Days {
			c := timeDay{Day: d.Day, Logged: make([]loggedTime, len(d.Logged)), Active: d.Active, Phone: d.Phone, Tickets: d.Tickets,
				CommitsMore: d.CommitsMore}
			for j, w := range d.Logged {
				c.Logged[j] = loggedTime{Key: w.Key, Hours: w.Hours, Title: clip(), Description: clip()}
			}
			for _, e := range d.Drafted {
				e.Reason = clip()
				c.Drafted = append(c.Drafted, e)
			}
			for _, q := range d.Questions {
				q.Reason = clip()
				c.Questions = append(c.Questions, q)
			}
			for _, cm := range d.Commits {
				c.Commits = append(c.Commits, commitLine{At: cm.At, Subject: clip()})
			}
			cut.Days[i] = c
		}
		return cut, nil
	}
	bare, _ := build(0)
	return fit("the time read", answerBudget-sizeOf(bare), build)
}

type timeDraftArgs struct {
	Entries []timeDraftArg `json:"entries"`
}

type timeDraftArg struct {
	Day    string   `json:"day"`
	Key    string   `json:"key"`
	Hours  float64  `json:"hours"`
	Reason string   `json:"reason"`
	Offer  []string `json:"offer"`
}

// entryKeyArg is time_draft's keys, as a refusal of one says it.
var entryKeyArg = keyParam{which: "entries holds a key", where: "in entries", naming: "entries holds a key naming"}

// TimeRow is what one time_draft call changed, for the thread's
// timeline: the call's ID, which names its row and its entries, the
// connection they are on, and their days.
type TimeRow struct {
	ID         string   `json:"call"`
	Connection string   `json:"connection"`
	Days       []string `json:"days"`
}

// draftDay is one day of a draft's answer, a receipt of the call: the
// entries it made on tickets, by ID and key in the order given, the IDs
// of the questions it made, the hours of the day's other entries not yet
// logged (another thread's, the person's, and those being posted), the
// tickets it skipped, and the hours already logged as this computer last
// read them, absent when it never read the day. Nothing the agent sent is
// echoed, so the answer of the most entries a call takes stays within
// answerBudget; time_read shows the entries whole.
type draftDay struct {
	Day        string      `json:"day"`
	Made       []madeEntry `json:"made,omitempty"`
	Questions  []int64     `json:"questions,omitempty"`
	OtherHours float64     `json:"other_hours,omitempty"`
	Skipped    []string    `json:"skipped,omitempty"`
	Logged     *float64    `json:"logged_hours,omitempty"`
}

type madeEntry struct {
	ID  int64  `json:"id"`
	Key string `json:"key"`
}

// draftAnswer is a draft's receipt. Site names the site of every key in
// it when the session has more than one connection; the keys are bare.
type draftAnswer struct {
	Site string     `json:"site,omitempty"`
	Days []draftDay `json:"days"`
	Note string     `json:"note"`
}

const (
	draftNote        = "Nothing is logged until the user approves it."
	draftSkippedNote = " A skipped key already has an entry of that day not yet logged; time_read shows it."
)

func (s *Server) timeDraft(ctx context.Context, caller Caller, raw json.RawMessage) (any, error) {
	var args timeDraftArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	offers := make([]int, len(args.Entries))
	for i, e := range args.Entries {
		offers[i] = len(e.Offer)
	}
	if err := trackerapp.CheckDraftShape(offers); err != nil {
		return nil, invalidf("%s", draftRefusal(err))
	}
	// The draft sends nothing to a tracker, so no approval applies: the
	// policy is read at admission and again just before the store write,
	// so a thread made read-only during the key lookup drafts nothing.
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
	// Each key, an entry's or an offer's, by its place.
	type keyAt struct {
		entry int
		offer int // -1 for the entry's own key
	}
	var keys []string
	var at []keyAt
	for i, e := range args.Entries {
		if k := strings.TrimSpace(e.Key); k != "" {
			keys, at = append(keys, k), append(at, keyAt{i, -1})
		}
		for j, o := range e.Offer {
			if strings.TrimSpace(o) == "" {
				return nil, invalidf("entries[%d].offer holds an empty key.", i)
			}
			keys, at = append(keys, o), append(at, keyAt{i, j})
		}
	}
	var found []ticket
	if len(keys) > 0 {
		if found, _, err = s.resolveKeys(ctx, caller.Thread, keys, entryKeyArg); err != nil {
			return nil, err
		}
	}
	name := func(k keyAt) string {
		if k.offer < 0 {
			return fmt.Sprintf("entries[%d].key", k.entry)
		}
		return fmt.Sprintf("entries[%d].offer", k.entry)
	}
	connection, site := "", ""
	for n, t := range found {
		i := slices.IndexFunc(sources, func(src trackerapp.TicketSource) bool { return src.ID == t.connection })
		if i < 0 {
			// The thread's linked ticket, on a connection no longer on.
			return nil, invalidf("%s %s is on a connection that is not turned on.", name(at[n]), keys[n])
		}
		switch src := sources[i]; {
		case src.Kind != jiraKind:
			return nil, invalidf("%s %s is on %s, which has no time tracking.", name(at[n]), keys[n], kindName(src.Kind))
		case connection == "":
			connection, site = t.connection, t.site
		case t.connection != connection:
			return nil, invalidf("%s %s is on %s and %s %s on %s; draft each site's time in its own call.",
				name(at[0]), keys[0], site, name(at[n]), keys[n], t.site)
		}
	}
	if connection == "" {
		if len(jira) > 1 {
			return nil, invalidf("No entry has a key, and this computer has %d Jira connections; give one entry a key on the site whose time this is.", len(jira))
		}
		connection, site = jira[0].ID, jira[0].Site
	}
	req := trackerapp.TimeDraftRequest{Thread: caller.Thread, Call: s.newID(), Entries: make([]trackerapp.TimeDraftEntry, len(args.Entries))}
	for i, e := range args.Entries {
		req.Entries[i] = trackerapp.TimeDraftEntry{Day: strings.TrimSpace(e.Day), Hours: e.Hours, Reason: e.Reason}
	}
	for n, t := range found {
		view := &trackerapp.TimeOfferView{Issue: t.issue, Key: t.key, Title: s.ticketTitle(t)}
		if k := at[n]; k.offer < 0 {
			req.Entries[k.entry].Ticket = view
		} else {
			req.Entries[k.entry].Offers = append(req.Entries[k.entry].Offers, *view)
		}
	}
	if _, err := s.h.WritePolicy(caller.Thread); err != nil {
		return nil, err
	}
	got, err := s.t.DraftTime(connection, req)
	if err != nil {
		if code, _, public := errorsx.PublicDetails(err); !public || code == CodeInvalidRequest {
			return nil, err
		}
		return nil, onSite(site, err)
	}
	if changed := slices.Concat(got.Made, got.Replaced); len(changed) > 0 {
		row := TimeRow{ID: req.Call, Connection: connection}
		for _, e := range changed {
			if !slices.Contains(row.Days, e.Day) {
				row.Days = append(row.Days, e.Day)
			}
		}
		slices.Sort(row.Days)
		s.h.Drafted(caller.Thread, row)
	}
	named := ""
	if len(sources) > 1 {
		named = site
	}
	return draftAnswerOf(got, named), nil
}

// draftRefusal is a draft refusal's sentence alone, after the sentinel
// trackerapp wraps it in.
func draftRefusal(err error) string {
	text := err.Error()
	if i := strings.LastIndex(text, trackerapp.ErrInvalidRequest.Error()+": "); i >= 0 {
		return text[i+len(trackerapp.ErrInvalidRequest.Error())+2:]
	}
	return text
}

// ticketTitle is t's title: the lookup's, or the mirror's card's, with no
// request; empty when neither has it.
func (s *Server) ticketTitle(t ticket) string {
	if t.title != "" {
		return t.title
	}
	v, ok, err := s.t.MirrorIssue(t.connection, t.issue)
	if err != nil || !ok {
		// A title only names the entry on the Time screen; its key does too.
		return ""
	}
	return v.Card.Title
}

func kindName(kind string) string {
	if kind == "linear" {
		return "Linear"
	}
	return kind
}

// draftAnswerOf is a draft's answer by day, naming site once when it is
// not empty.
func draftAnswerOf(v trackerapp.TimeDraftView, site string) draftAnswer {
	out := draftAnswer{Site: site, Days: []draftDay{}, Note: draftNote}
	day := func(d string) *draftDay {
		i := slices.IndexFunc(out.Days, func(x draftDay) bool { return x.Day == d })
		if i < 0 {
			out.Days = append(out.Days, draftDay{Day: d})
			i = len(out.Days) - 1
		}
		return &out.Days[i]
	}
	for _, l := range v.Logged {
		d := day(l.Day)
		if !l.Unread {
			hours := float64(l.Minutes) / 60
			d.Logged = &hours
		}
	}
	for _, e := range v.Made {
		d := day(e.Day)
		if e.Key == "" {
			d.Questions = append(d.Questions, e.ID)
			continue
		}
		d.Made = append(d.Made, madeEntry{ID: e.ID, Key: e.Key})
	}
	for _, e := range v.Kept {
		day(e.Day).OtherHours += float64(e.Minutes) / 60
	}
	for _, e := range v.Skipped {
		day(e.Day).Skipped = append(day(e.Day).Skipped, e.Key)
	}
	if len(v.Skipped) > 0 {
		out.Note += draftSkippedNote
	}
	slices.SortFunc(out.Days, func(a, b draftDay) int { return strings.Compare(a.Day, b.Day) })
	return out
}

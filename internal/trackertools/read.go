package trackertools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

type readArgs struct {
	Key     string `json:"key"`
	History bool   `json:"history"`
	More    string `json:"more"`
}

// maxKeyRunes bounds a key: longer text, a site and a key included, names
// no ticket on either tracker, and a refusal quotes the key back.
const maxKeyRunes = 128

// ticket is where a key resolved: the connection that reads it, its site
// as a person names it, the tracker's issue ID, and its key as the tracker
// writes it, with no site.
type ticket struct {
	connection, site, issue, key string
	// linked is the thread's own ticket.
	linked bool
}

// What an answer shows of a ticket whole or cut, in bytes of JSON.
const (
	// commentsShown is the most comments a first answer holds, the latest.
	commentsShown = 10
	// fieldHead is the most of one field's value a first answer or an
	// answer of more fields holds; the rest has its own continuation, which
	// takes a whole answer.
	fieldHead = 2 << 10
	// listedComment is the most of one comment's body an answer of older
	// comments holds; the rest has its own continuation.
	listedComment = 4 << 10
	// minPiece is the least of a text worth showing: a part with less room
	// than this waits for its continuation.
	minPiece = 256
	// titleCut, refCut and changeCut cut, with no continuation, a ticket's
	// title, another ticket's title (a search row's too) or any name (a
	// status, a type, a priority, a person, an attachment, a label), and
	// each value of a change in its history. Titles and names are shorter
	// on both trackers; a change can hold a whole old description, which
	// the description itself shows as it is now.
	titleCut  = 1 << 10
	refCut    = 300
	changeCut = 1 << 10
	// refusalCut cuts a source's refusal in a search's unsearched: the
	// trackers' sentences are a line.
	refusalCut = 1 << 10
)

// The parts an answer can leave for a continuation, as a cursor names them.
const (
	partDescription = "d"
	partComment     = "b" // the rest of one comment's body
	partComments    = "c" // older comments
	partFields      = "f" // fields not shown
	partField       = "v" // the rest of one field's value
	partLinks       = "l"
	partAttachments = "a"
	partLabels      = "t"
	partHistory     = "h"
)

// moreCursor is one part an answer left out: the ticket by connection and
// issue, its update time when the answer was made, the part, and where in
// it the rest starts (a list index, an item of a field's value, a byte
// offset into a text, the tracker's comment page, and a comment's ID and
// edit time).
//
// What a continuation keeps true. A value is a client's text: it may be
// one an answer gave for a ticket that has changed since, or one nobody
// gave. Nothing in it is used to read, index or slice until it has been
// checked against the ticket as it is now:
//
//   - its connection is on (refused saying so otherwise);
//   - the ticket's update time is the value's;
//   - for a comment's rest, the comment is on its page with the value's
//     edit time, since on Linear an edit leaves the ticket's update time;
//   - each position is inside what the part holds now: a list index, a
//     field's index or its value's item below their counts, an index into
//     a page of comments below the page's count (0 always), and a text
//     offset below the text's length and at the start of a rune.
//
// A value that fails a check is refused before any of it is used, with
// changedSince, and the model reads the ticket again. A value no answer
// could hold (malformed, an unknown part, a member missing or below zero)
// is refused as not one these tools gave. A member a part does not use is
// never read. continued makes the checks; textClip relies on them.
type moreCursor struct {
	Connection string `json:"c"`
	Issue      string `json:"i"`
	Updated    int64  `json:"u"`
	Part       string `json:"p"`
	Index      int    `json:"n,omitempty"`
	Item       int    `json:"m,omitempty"`
	Offset     int    `json:"o,omitempty"`
	Page       string `json:"g,omitempty"`
	Comment    string `json:"x,omitempty"`
	Edited     int64  `json:"e,omitempty"`
}

// changedSince is the refusal of a value the ticket as it is now cannot
// honor, for the ticket's key.
func changedSince(key string) error {
	return invalidf("more is from an earlier version of %s, which has changed since; read it again.", key)
}

// notOurs is the refusal of a value no answer could hold.
func notOurs() error { return invalidf("more is not one these tools gave; read the ticket again.") }

// atText reports whether offset is where the rest of text can start: in
// it and at a rune's start.
func atText(text string, offset int) bool {
	return offset >= 0 && offset < len(text) && utf8.RuneStart(text[offset])
}

type readComment struct {
	By       string `json:"by,omitempty"`
	At       string `json:"at"`
	Body     string `json:"body"`
	Internal bool   `json:"internal,omitempty"`
}

type readLink struct {
	Relation string `json:"relation"`
	Key      string `json:"key"`
	Title    string `json:"title,omitempty"`
	Status   string `json:"status,omitempty"`
}

type readChange struct {
	At    string `json:"at"`
	By    string `json:"by,omitempty"`
	Field string `json:"field"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// readAnswer is one ticket, or the parts of it a continuation names. No
// member is empty: an empty one is left out, so the model never reads a
// field as cleared that the ticket simply does not have. Fields holds every
// filled field the members above do not, in the tracker's order. More is
// what the answer left out, each a sentence saying what and how much, with
// the value that gets it.
type readAnswer struct {
	Key          string            `json:"key"`
	Title        string            `json:"title,omitempty"`
	Status       string            `json:"status,omitempty"`
	Type         string            `json:"type,omitempty"`
	Assignee     string            `json:"assignee,omitempty"`
	Priority     string            `json:"priority,omitempty"`
	Estimate     string            `json:"estimate,omitempty"`
	Created      string            `json:"created,omitempty"`
	Updated      string            `json:"updated,omitempty"`
	Resolved     string            `json:"resolved,omitempty"`
	Labels       []string          `json:"labels,omitempty"`
	Parent       string            `json:"parent,omitempty"`
	URL          string            `json:"url,omitempty"`
	Description  string            `json:"description,omitempty"`
	Fields       json.RawMessage   `json:"fields,omitempty"`
	Links        []readLink        `json:"links,omitempty"`
	Attachments  []string          `json:"attachments,omitempty"`
	Comments     []readComment     `json:"comments,omitempty"`
	History      []readChange      `json:"history,omitempty"`
	HistorySince string            `json:"history_since,omitempty"`
	More         map[string]string `json:"more,omitempty"`
}

func (s *Server) read(ctx context.Context, threadID string, raw json.RawMessage) (any, error) {
	var args readArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.More != "" {
		if args.Key != "" || args.History {
			return nil, invalidf("more answers that part alone; leave out key and history.")
		}
		return s.readMore(ctx, args.More)
	}
	t, sources, err := s.resolve(ctx, threadID, args.Key)
	if err != nil {
		return nil, err
	}
	d, err := s.t.IssueDetail(ctx, t.connection, t.issue)
	switch {
	case notFound(err) && t.linked:
		return nil, errorsx.Public(CodeNotFound, fmt.Sprintf(
			"%s, the ticket this thread is linked to, is not on %s any more, or the user's account cannot see it.", t.key, t.site), err)
	case notFound(err):
		return nil, errorsx.Public(CodeNotFound, fmt.Sprintf("No ticket %s on %s.", t.key, t.site), err)
	case err != nil:
		return nil, err
	}
	sh := newSheet(s, t.connection, qualifier(sources, t.site), d)
	return sh.first(args.History)
}

// qualifier is how answers write the keys of site's tickets: as the
// tracker does when one connection is on, and as site/KEY when several
// are, so every key an answer gives opens that ticket again.
func qualifier(sources []trackerapp.TicketSource, site string) func(string) string {
	if len(sources) < 2 {
		return func(key string) string { return key }
	}
	return func(key string) string {
		if key == "" {
			return ""
		}
		return site + "/" + key
	}
}

// resolve is the ticket key names for thread threadID, and the sources on:
// with no key, the ticket the thread is linked to; a key the link names,
// the link's (no request); site/KEY, that site's ticket; any other key,
// the ticket of the one site whose tracker finds it, refused when several
// sites do.
func (s *Server) resolve(ctx context.Context, threadID, key string) (ticket, []trackerapp.TicketSource, error) {
	key = strings.TrimSpace(key)
	if len([]rune(key)) > maxKeyRunes {
		return ticket{}, nil, invalidf("key is longer than any ticket key.")
	}
	site, bare, qualified := strings.Cut(key, "/")
	if !qualified {
		site, bare = "", key
	}
	bare = strings.ToUpper(strings.TrimSpace(bare))
	link, linked, err := s.t.LinkedTicket(threadID)
	var refused *trackerapp.LinkRefusal
	switch {
	case key == "" && err != nil:
		return ticket{}, nil, err
	case key == "" && !linked:
		return ticket{}, nil, errorsx.Public(CodeNoTicket, "This thread is not linked to a ticket; name one with key.", nil)
	case errors.As(err, &refused):
		// The thread's ticket cannot be read here, which says nothing of
		// the ticket key names: the sources look for it.
		linked = false
	case err != nil:
		return ticket{}, nil, err
	}
	sources, err := s.sources()
	if err != nil {
		return ticket{}, nil, err
	}
	linkTicket := ticket{connection: link.Connection, site: link.Site, issue: link.Issue, linked: true}
	if key == "" || linked && strings.EqualFold(link.Key, bare) && (!qualified || strings.EqualFold(link.Site, strings.TrimSpace(site))) {
		linkTicket.key = link.Key
		return linkTicket, sources, nil
	}
	if bare == "" {
		return ticket{}, nil, invalidf("key names a site and no ticket; write it as site/KEY.")
	}
	asked := sources
	if qualified {
		asked = slices.DeleteFunc(slices.Clone(sources), func(src trackerapp.TicketSource) bool { return !strings.EqualFold(src.Site, strings.TrimSpace(site)) })
		if len(asked) == 0 {
			return ticket{}, nil, errorsx.Public(CodeNotFound, fmt.Sprintf(
				"No connection on this computer reaches %s; the sites on are %s.", strings.TrimSpace(site), siteList(sources)), nil)
		}
	}
	// One hit a site: the first of the site's connections, in order, that
	// has the key. Two accounts on one site reaching a key reach one ticket.
	var hits []ticket
	var failed error
	for _, src := range asked {
		if slices.ContainsFunc(hits, func(h ticket) bool { return h.site == src.Site }) {
			continue
		}
		found, err := s.t.Lookup(ctx, src.ID, []string{bare})
		if err != nil {
			// As ticket_find: a refusal the model can read leaves the other
			// sources asked; anything else fails the call.
			if _, _, public := errorsx.PublicDetails(err); !public || ctx.Err() != nil {
				return ticket{}, nil, onSite(src.Site, err)
			}
			if failed == nil {
				failed = onSite(src.Site, err)
			}
			continue
		}
		if len(found.Found) > 0 {
			hits = append(hits, ticket{connection: src.ID, site: src.Site, issue: found.Found[0].Card.ID, key: bare})
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0], sources, nil
	case len(hits) > 1:
		sites, keys := make([]string, len(hits)), make([]string, len(hits))
		for i, h := range hits {
			sites[i], keys[i] = h.site, qualifier(sources, h.site)(h.key)
		}
		return ticket{}, nil, errorsx.Public(CodeAmbiguous, fmt.Sprintf("%s is on %s; name one: %s.", bare, sayList(sites, "and"), sayList(keys, "or")), nil)
	case failed != nil:
		// A source that could not answer may hold the key.
		return ticket{}, nil, failed
	}
	return ticket{}, nil, errorsx.Public(CodeNotFound, fmt.Sprintf("No ticket %s on %s.", bare, siteList(asked)), nil)
}

// readMore answers the part a value of an earlier answer's more names,
// when the ticket as it is now honors it (moreCursor).
func (s *Server) readMore(ctx context.Context, text string) (any, error) {
	var c moreCursor
	if err := decodeCursor("more", text, &c); err != nil {
		return nil, err
	}
	if c.Connection == "" || c.Issue == "" || c.Part == "" || c.Index < 0 || c.Item < 0 || c.Offset < 0 {
		return nil, notOurs()
	}
	sources, err := s.sources()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(sources, func(src trackerapp.TicketSource) bool { return src.ID == c.Connection })
	if i < 0 {
		return nil, invalidf("more is from a connection that is no longer on; read the ticket again.")
	}
	d, err := s.t.IssueDetail(ctx, c.Connection, c.Issue)
	switch {
	case notFound(err):
		return nil, errorsx.Public(CodeNotFound, "That ticket is not there any more, or the user's account cannot see it.", err)
	case err != nil:
		return nil, err
	}
	sh := newSheet(s, c.Connection, qualifier(sources, sources[i].Site), d)
	if d.Card.Updated != c.Updated {
		return nil, changedSince(sh.key(d.Card.Key))
	}
	return sh.continued(ctx, c)
}

// sheet is one read of a ticket as its answers show it: the detail, its
// filled fields and its history in the answers' terms, the comment pages
// read for it, and the answer being made.
type sheet struct {
	s          *Server
	connection string
	key        func(string) string
	d          trackerapp.DetailView
	fields     []shownField
	history    []readChange
	pages      map[string]trackerapp.CommentPageView
	out        readAnswer
}

// shownField is a filled field: its name, told apart by its ID when two
// share it, and its values, one of them for a field that holds one.
type shownField struct {
	name   string
	values []any
	scalar bool
}

func newSheet(s *Server, connection string, key func(string) string, d trackerapp.DetailView) *sheet {
	sh := &sheet{s: s, connection: connection, key: key, d: d, pages: map[string]trackerapp.CommentPageView{}}
	names := map[string]int{}
	for _, f := range d.Fields {
		if f.Value.Kind != "" {
			names[f.Name]++
		}
	}
	for _, f := range d.Fields {
		if f.Value.Kind == "" {
			continue
		}
		name := f.Name
		if names[name] > 1 {
			name += " (" + f.ID + ")"
		}
		values := fieldValues(f.Value)
		sh.fields = append(sh.fields, shownField{name: name, values: values, scalar: !f.List && len(values) == 1})
	}
	changes := slices.Clone(d.History)
	slices.SortStableFunc(changes, func(a, b trackerapp.EventView) int { return cmp.Compare(b.At, a.At) })
	for _, e := range changes {
		from, _ := textClip(e.From, 0, changeCut)
		to, _ := textClip(e.To, 0, changeCut)
		ch := readChange{At: stamp(e.At), Field: name(e.Field), From: from, To: to}
		if e.Author != nil {
			ch.By = name(e.Author.Name)
		}
		sh.history = append(sh.history, ch)
	}
	return sh
}

// name is a name or another ticket's title as answers show it, cut at
// refCut.
func name(text string) string {
	shown, _ := textClip(text, 0, refCut)
	return shown
}

// fieldValues is a field's value as texts and numbers.
func fieldValues(v trackerapp.FieldValueView) []any {
	var values []any
	for _, t := range v.Texts {
		values = append(values, t)
	}
	for _, n := range v.Numbers {
		values = append(values, n)
	}
	for _, d := range v.Dates {
		values = append(values, d)
	}
	for _, t := range v.Times {
		values = append(values, stamp(t))
	}
	for _, c := range v.Choices {
		values = append(values, c.Name)
	}
	return values
}

// section is one part of a first answer that can be cut: its weight in
// the share of the bytes the card leaves, and fill, which shows it within
// allow bytes and returns the bytes it showed.
type section struct {
	weight int
	fill   func(allow int) int
}

// first is the first answer of the ticket: the card whole, and each other
// part within its share of what the card leaves, so no long part pushes
// another out. A part's share is in proportion to its weight; what a part
// does not need goes to the parts that want more, the description first.
func (sh *sheet) first(history bool) (readAnswer, error) {
	d, c := sh.d, sh.d.Card
	sh.out = readAnswer{
		Key: sh.key(c.Key), Status: name(c.Status.Name), URL: d.URL, Estimate: estimate(d.Rows),
		Created: stampIf(c.Created), Updated: stampIf(c.Updated), Resolved: stampIf(c.Resolved),
	}
	sh.out.Title, _ = textClip(c.Title, 0, titleCut)
	if c.Type != nil {
		sh.out.Type = name(c.Type.Name)
	}
	if c.Assignee != nil {
		sh.out.Assignee = name(c.Assignee.Name)
	}
	if c.Priority != nil {
		sh.out.Priority = name(c.Priority.Name)
	}
	if c.Parent != nil {
		sh.out.Parent, _ = textClip(strings.TrimSpace(sh.key(c.Parent.Key)+" "+c.Parent.Title), 0, refCut)
	}
	if history && d.HistoryFrom > 0 {
		sh.out.HistorySince = stamp(d.HistoryFrom)
	}
	head := sh.out
	sections := []section{
		{3, func(allow int) int { return sh.description(0, allow) }},
		{4, func(allow int) int { return sh.latestComments(allow) }},
		{2, func(allow int) int { return sh.fieldsFrom(0, 0, 0, allow, len(sh.fields), fieldHead) }},
		{1, func(allow int) int { return sh.list(partLinks, 0, allow) }},
		{1, func(allow int) int { return sh.list(partAttachments, 0, allow) }},
		{1, func(allow int) int { return sh.list(partLabels, 0, allow) }},
	}
	if history {
		sections = append(sections, section{2, func(allow int) int { return sh.list(partHistory, 0, allow) }})
	}
	demands, weights := make([]int, len(sections)), make([]int, len(sections))
	for i, sec := range sections {
		sh.out = head
		demands[i], weights[i] = sec.fill(math.MaxInt), sec.weight
	}
	return sh.fit(head, func(pool int) error {
		for i, share := range allot(demands, weights, pool) {
			sections[i].fill(share)
		}
		return nil
	})
}

// continued is the answer to a continuation: the part c names, from where
// c says, as much as fits, and at least a piece of it. It first checks c
// against the ticket as it is now, as moreCursor says.
func (sh *sheet) continued(ctx context.Context, c moreCursor) (readAnswer, error) {
	head := readAnswer{Key: sh.key(sh.d.Card.Key)}
	stale := changedSince(head.Key)
	var fill func(allow int) error
	switch c.Part {
	case partDescription:
		if !atText(sh.d.Description, c.Offset) {
			return readAnswer{}, stale
		}
		fill = func(allow int) error { sh.description(c.Offset, allow); return nil }
	case partComment:
		page, err := sh.commentPage(ctx, c.Page)
		if err != nil {
			return readAnswer{}, err
		}
		i := slices.IndexFunc(page.Comments, func(cm trackerapp.CommentView) bool { return cm.ID == c.Comment })
		if i < 0 || page.Comments[i].Updated != c.Edited || !atText(page.Comments[i].Body, c.Offset) {
			return readAnswer{}, stale
		}
		fill = func(allow int) error { sh.comment(page.Comments[i], c.Page, c.Offset, allow-1); return nil }
	case partComments:
		page, err := sh.commentPage(ctx, c.Page)
		if err != nil {
			return readAnswer{}, err
		}
		if c.Index > 0 && c.Index >= len(page.Comments) {
			return readAnswer{}, stale
		}
		fill = func(allow int) error { return sh.olderComments(ctx, c.Page, c.Index, allow) }
	case partFields:
		if c.Index >= len(sh.fields) {
			return readAnswer{}, stale
		}
		fill = func(allow int) error { sh.fieldsFrom(c.Index, 0, 0, allow, len(sh.fields), fieldHead); return nil }
	case partField:
		if c.Index >= len(sh.fields) || c.Item >= len(sh.fields[c.Index].values) {
			return readAnswer{}, stale
		}
		text, isText := sh.fields[c.Index].values[c.Item].(string)
		if isText && !atText(text, c.Offset) || !isText && c.Offset != 0 {
			return readAnswer{}, stale
		}
		// The rest of one field is the answer's one part: it takes all the
		// answer holds.
		fill = func(allow int) error { sh.fieldsFrom(c.Index, c.Item, c.Offset, allow, c.Index+1, allow); return nil }
	case partLinks, partAttachments, partLabels, partHistory:
		if items, _ := sh.items(c.Part); c.Index >= len(items) {
			return readAnswer{}, stale
		}
		fill = func(allow int) error { sh.list(c.Part, c.Index, allow); return nil }
	default:
		return readAnswer{}, notOurs()
	}
	return sh.fit(head, fill)
}

// fit fills an answer from head within answerBudget: fill gets the bytes
// head leaves, and less while the answer it makes, continuations
// included, is over the budget.
func (sh *sheet) fit(head readAnswer, fill func(pool int) error) (readAnswer, error) {
	return fit(head.Key, answerBudget-sizeOf(head), func(pool int) (readAnswer, error) {
		sh.out = head
		err := fill(pool)
		return sh.out, err
	})
}

// rest records a part the answer leaves for a continuation.
func (sh *sheet) rest(label string, c moreCursor) {
	if sh.out.More == nil {
		sh.out.More = map[string]string{}
	}
	c.Connection, c.Issue, c.Updated = sh.connection, sh.d.Card.ID, sh.d.Card.Updated
	// Two comments by one person in one minute, cut to the same size, are
	// told apart.
	key := label
	for n := 2; sh.out.More[key] != ""; n++ {
		key = fmt.Sprintf("%s #%d", label, n)
	}
	sh.out.More[key] = encodeCursor(c)
}

// description shows the description from byte from within allow bytes.
func (sh *sheet) description(from, allow int) int {
	text := sh.d.Description
	if from >= len(text) {
		return 0
	}
	shown, next := textClip(text, from, allow)
	sh.out.Description = shown
	if next < len(text) {
		label := "the rest of the description"
		if shown == "" && from == 0 {
			label = "the description"
		}
		sh.rest(fmt.Sprintf("%s (%s)", label, sizeLabel(len(text)-next)), moreCursor{Part: partDescription, Offset: next})
	}
	if shown == "" {
		return 0
	}
	return jsonLen(shown) + 2
}

// latestComments shows the latest comments of the ticket's first page
// within allow bytes, each within an equal share of them, the share a
// comment does not need going to the others, newest first. It shows fewer
// comments when allow cannot hold each with a piece of its body.
func (sh *sheet) latestComments(allow int) int {
	page := sh.d.Comments
	n := min(len(page), commentsShown)
	for ; n > 0; n-- {
		least := 0
		for _, c := range page[:n] {
			least += commentHead(c) + min(minPiece, jsonLen(c.Body)) + 1
		}
		if least <= allow {
			break
		}
	}
	demands, weights := make([]int, n), make([]int, n)
	for i := range n {
		demands[i], weights[i] = sizeOf(readCommentOf(page[i], page[i].Body))+1, 1
	}
	used := 0
	for i, share := range allot(demands, weights, allow) {
		used += sh.comment(page[i], "", 0, share-1) + 1
	}
	switch {
	case n < len(page):
		sh.olderAt(trackerapp.CommentPageView{Comments: page, Next: sh.d.MoreComments}, "", n)
	case sh.d.MoreComments != "":
		sh.rest("older comments", moreCursor{Part: partComments, Page: sh.d.MoreComments})
	}
	return used
}

// comment shows one comment of the page token names, its body from byte
// from within allow bytes for the comment, and returns the bytes shown.
func (sh *sheet) comment(c trackerapp.CommentView, token string, from, allow int) int {
	body, next := textClip(c.Body, from, allow-commentHead(c)+2)
	shown := readCommentOf(c, body)
	sh.out.Comments = append(sh.out.Comments, shown)
	if next < len(c.Body) {
		label := "the rest of the comment"
		if body == "" && from == 0 {
			label = "the comment"
		}
		sh.rest(fmt.Sprintf("%s by %s at %s (%s)", label, cmp.Or(shown.By, "someone"), shown.At, sizeLabel(len(c.Body)-next)),
			moreCursor{Part: partComment, Page: token, Comment: c.ID, Edited: c.Updated, Offset: next})
	}
	return sizeOf(shown)
}

// commentHead is the bytes a comment takes with an empty body, the
// quotes of its body included.
func commentHead(c trackerapp.CommentView) int { return sizeOf(readCommentOf(c, "")) }

func readCommentOf(c trackerapp.CommentView, body string) readComment {
	return readComment{By: name(c.Author.Name), At: stamp(c.Created), Body: body, Internal: c.Internal}
}

// commentPage is the page of the ticket's comments token names: the
// detail's first page for none, any other read once.
func (sh *sheet) commentPage(ctx context.Context, token string) (trackerapp.CommentPageView, error) {
	if token == "" {
		return trackerapp.CommentPageView{Comments: sh.d.Comments, Next: sh.d.MoreComments}, nil
	}
	if page, ok := sh.pages[token]; ok {
		return page, nil
	}
	page, err := sh.s.t.Comments(ctx, sh.connection, sh.d.Card.ID, token)
	if err != nil {
		return trackerapp.CommentPageView{}, err
	}
	sh.pages[token] = page
	return page, nil
}

// olderComments shows comments from the index-th of the page token
// names, in order, each body within listedComment, while allow holds
// them; it goes on to the following pages, each read once, while room is
// left.
func (sh *sheet) olderComments(ctx context.Context, token string, index, allow int) error {
	used := 0
	for {
		page, err := sh.commentPage(ctx, token)
		if err != nil {
			return err
		}
		for ; index < len(page.Comments); index++ {
			c := page.Comments[index]
			room := allow - used - 1
			if used > 0 && room < commentHead(c)+min(minPiece, jsonLen(c.Body)) {
				sh.olderAt(page, token, index)
				return nil
			}
			used += sh.comment(c, token, 0, min(room, commentHead(c)+listedComment)) + 1
		}
		if page.Next == "" {
			return nil
		}
		if allow-used < minPiece*2 {
			sh.rest("older comments", moreCursor{Part: partComments, Page: page.Next})
			return nil
		}
		token, index = page.Next, 0
	}
}

// olderAt records the older comments from the index-th of page, whose
// token is token.
func (sh *sheet) olderAt(page trackerapp.CommentPageView, token string, index int) {
	left := len(page.Comments) - index
	label := fmt.Sprintf("%d older comments", left)
	if page.Next != "" {
		label = fmt.Sprintf("older comments (more than %d)", left)
	}
	sh.rest(label, moreCursor{Part: partComments, Page: token, Index: index})
}

// fieldsFrom shows the fields from the index-th to the one before end,
// that one from its item-th value and that value's byte from, each value
// within each bytes, while allow holds them, as one JSON object in the
// tracker's order.
func (sh *sheet) fieldsFrom(index, item, from, allow, end, each int) int {
	var b bytes.Buffer
	left := allow - 1 // the braces, less the comma the first field has not
	for i := index; i < end; i++ {
		f := sh.fields[i]
		head := len(mustJSON(f.name)) + 2 // the colon and the comma
		least := sizeOf(f.values)
		if f.scalar {
			least = sizeOf(f.values[0])
		}
		if i > index && left < head+min(minPiece, least) {
			sh.rest(fmt.Sprintf("%d more fields", len(sh.fields)-i), moreCursor{Part: partFields, Index: i})
			break
		}
		start, at := 0, 0
		if i == index {
			start, at = item, from
		}
		value, used := sh.fieldValue(i, start, at, min(left-head, each))
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.Write(mustJSON(f.name))
		b.WriteByte(':')
		b.Write(value)
		left -= head + used
	}
	if b.Len() == 0 {
		return 0
	}
	sh.out.Fields = append(append([]byte{'{'}, b.Bytes()...), '}')
	return len(sh.out.Fields)
}

// fieldValue is the i-th field's value from its item-th value and that
// value's byte from, within allow bytes, and the bytes it takes. A value
// cut short leaves the rest of the field for a continuation.
func (sh *sheet) fieldValue(i, item, from, allow int) (json.RawMessage, int) {
	f := sh.fields[i]
	scalar := f.scalar && item == 0
	used := 2 // a list's brackets
	if scalar {
		used = 0
	}
	var shown []any
	for j := item; j < len(f.values); j++ {
		start, comma := 0, min(len(shown), 1)
		if j == item {
			start = from
		}
		room := allow - used - comma
		text, isText := f.values[j].(string)
		if !isText {
			size := sizeOf(f.values[j])
			if size > room && len(shown) > 0 {
				sh.fieldRest(i, j, start, 0)
				break
			}
			shown, used = append(shown, f.values[j]), used+size+comma
			continue
		}
		piece, next := textClip(text, start, room)
		if piece == "" && len(shown) > 0 {
			sh.fieldRest(i, j, start, 0)
			break
		}
		shown, used = append(shown, piece), used+len(mustJSON(piece))+comma
		if next < len(text) {
			sh.fieldRest(i, j, next, len(text)-next)
			break
		}
	}
	raw := mustJSON(shown)
	if scalar && len(shown) == 1 {
		raw = mustJSON(shown[0])
	}
	return raw, len(raw)
}

// fieldRest records the rest of the i-th field, from its j-th value and
// that value's byte from, cut bytes of that value being left.
func (sh *sheet) fieldRest(i, j, from, cutBytes int) {
	f := sh.fields[i]
	left := cutBytes
	for _, v := range f.values[j+min(1, cutBytes):] {
		left += sizeOf(v)
	}
	sh.rest(fmt.Sprintf("the rest of %s (%s)", f.name, sizeLabel(left)), moreCursor{Part: partField, Index: i, Item: j, Offset: from})
}

// list shows the items of part from the index-th, whole, while allow holds
// them. Every item is a few KB at most (titles and names cut at refCut,
// change values at changeCut), so a continuation's allow always holds its
// first.
func (sh *sheet) list(part string, index, allow int) int {
	items, what := sh.items(part)
	used := 0
	for i := index; i < len(items); i++ {
		size := sizeOf(items[i]) + 1
		if used+size > allow {
			sh.rest(fmt.Sprintf("%d %s", len(items)-i, what), moreCursor{Part: part, Index: i})
			break
		}
		used += size
		switch part {
		case partLinks:
			sh.out.Links = append(sh.out.Links, items[i].(readLink))
		case partAttachments:
			sh.out.Attachments = append(sh.out.Attachments, items[i].(string))
		case partLabels:
			sh.out.Labels = append(sh.out.Labels, items[i].(string))
		case partHistory:
			sh.out.History = append(sh.out.History, items[i].(readChange))
		}
	}
	return used
}

// items are part's items as answers show them, and what more of them are
// called.
func (sh *sheet) items(part string) ([]any, string) {
	var out []any
	switch part {
	case partLinks:
		for _, l := range sh.d.Links {
			out = append(out, readLink{Relation: name(l.Relation), Key: sh.key(l.Issue.Key), Title: name(l.Issue.Title), Status: name(l.Issue.Status.Name)})
		}
		return out, "more links"
	case partAttachments:
		for _, a := range sh.d.Attachments {
			out = append(out, name(a.Name))
		}
		return out, "more attachments"
	case partLabels:
		for _, l := range sh.d.Card.Labels {
			if l.Name != "" {
				out = append(out, name(l.Name))
			}
		}
		return out, "more labels"
	}
	for _, ch := range sh.history {
		out = append(out, ch)
	}
	return out, "older changes"
}

// estimate is the ticket's estimate as its screen shows it with no board
// in view: the first board row's, in points, or as a time in minutes
// under an hour and hours from one.
func estimate(rows []trackerapp.TicketRowView) string {
	if len(rows) == 0 || rows[0].Estimate == nil {
		return ""
	}
	v := *rows[0].Estimate
	switch rows[0].Unit {
	case "points":
		return trimNumber(v) + " points"
	case "time":
		if v < 3600 {
			return strconv.Itoa(int(math.Round(v/60))) + "m"
		}
		return trimNumber(v/3600) + "h"
	}
	return ""
}

// trimNumber is n as the ticket screen writes an estimate: whole, or to
// one decimal place.
func trimNumber(n float64) string {
	if n == math.Trunc(n) {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return strings.TrimSuffix(strconv.FormatFloat(n, 'f', 1, 64), ".0")
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// Field names and values are strings and finite numbers.
		panic("trackertools: encode a field: " + err.Error())
	}
	return raw
}

// stamp is a Unix-milliseconds time as the model reads it: UTC, to the
// minute.
func stamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04Z")
}

// stampIf is stamp, or nothing for no time.
func stampIf(ms int64) string {
	if ms == 0 {
		return ""
	}
	return stamp(ms)
}

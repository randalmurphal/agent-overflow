package trackertools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// ticket_create makes one ticket, through a board of its project this
// computer mirrors (Host.CreateBoard), so the new ticket is read into the
// board as a person's create is. Every field of set but the status, the
// sprint and the estimate is part of the create, which both trackers
// check before anything is made: a value refused there leaves nothing
// created. The status, sprint and estimate are the ticket's own steps
// after it exists (ticket_update's), and one refused says the ticket was
// created. A create is recorded before it is sent (trackerapp.ByAgent),
// so the same call after an answer that never came finds the ticket
// rather than making a second.

type createArgs struct {
	Project     string                     `json:"project"`
	Title       string                     `json:"title"`
	Type        string                     `json:"type"`
	Description string                     `json:"description"`
	Set         map[string]json.RawMessage `json:"set"`
}

type createAnswer struct {
	Key     string `json:"key"`
	Note    string `json:"note,omitempty"`
	Refused string `json:"refused,omitempty"`
	Unknown string `json:"unknown,omitempty"`
}

// createTarget is where a create goes: the connection, the board and
// the project's key and ID, with the sources on.
type createTarget struct {
	src     trackerapp.TicketSource
	board   trackerapp.BoardSummary
	boardID string
	key     string
	sources []trackerapp.TicketSource
}

func (s *Server) create(ctx context.Context, caller Caller, raw json.RawMessage) (any, error) {
	var args createArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return nil, invalidf("title is empty; give the ticket one.")
	}
	set, err := parseSet(args.Set)
	if err != nil {
		return nil, err
	}
	if err := s.h.Writable(caller.Thread); err != nil {
		return nil, err
	}
	tg, err := s.createTarget(caller.Thread, args.Project)
	if err != nil {
		return nil, err
	}
	typeID, err := s.issueType(ctx, tg, strings.TrimSpace(args.Type))
	if err != nil {
		return nil, err
	}
	fields, after, err := s.createFields(ctx, tg, typeID, set)
	if err != nil {
		return nil, err
	}
	if args.Description != "" {
		fields = append(fields, trackerapp.FieldEditInput{Field: string(tracker.FieldDescription), Markdown: &trackerapp.MarkdownEditInput{Markdown: args.Description}})
	}
	id := s.newID()
	wctx := trackerapp.ByAgent(context.WithoutCancel(ctx), caller.Thread, id)
	if key, found, err := s.repeatCreate(wctx, caller, tg, typeID, title); err != nil || found {
		if err != nil {
			return nil, err
		}
		return createAnswer{Key: key, Note: "Your earlier call created it; it was not created again."}, nil
	}
	v, err := s.t.Create(wctx, tg.src.ID, trackerapp.CreateRequest{Board: tg.boardID, Project: tg.board.Project, Type: typeID, Title: title, Fields: fields})
	if err != nil {
		if _, msg, ok := errorsx.PublicDetails(err); ok && strings.Contains(msg, "may or may not") {
			return nil, createUnknown(tg.src.Kind)
		}
		return nil, err
	}
	if v.CreateUnknown || v.Created == "" && slices.ContainsFunc(v.Parts, func(p trackerapp.PartView) bool { return p.State == "unknown" }) {
		return nil, createUnknown(tg.src.Kind)
	}
	if v.Created == "" {
		return nil, createRefused(tg, v)
	}
	d, err := s.t.IssueDetail(wctx, tg.src.ID, v.Created)
	if err != nil {
		return nil, fmt.Errorf("trackertools: read the ticket created as %s: %w", v.Created, err)
	}
	out := createAnswer{Key: qualifier(tg.sources, tg.src.Site)(d.Card.Key)}
	if v.NotOnBoard != "" {
		out.Note = fmt.Sprintf("Board %s does not show it: its filter leaves it out.", name(tg.board.Name))
	}
	written := WrittenTicket{Connection: tg.src.ID, Issue: d.Card.ID, Key: out.Key, Title: d.Card.Title, Applied: []string{"created"}}
	// A part after the create (an image the description embeds) is the
	// created ticket's.
	if state, sentence := judge(tg.src.Kind, "description", "change", trackerapp.WriteView{Parts: slices.DeleteFunc(slices.Clone(v.Parts),
		func(p trackerapp.PartView) bool { return p.Step == "create" })}, nil); state != tracker.StateApplied {
		out.Refused = createdBut(out.Key, sentence)
	}
	if len(after) > 0 && out.Refused == "" {
		p := &planned{t: ticket{connection: tg.src.ID, site: tg.src.Site, issue: d.Card.ID, key: d.Card.Key}, key: out.Key, src: tg.src, d: d}
		for _, nv := range after {
			if err := s.planStep(caller, p, nv); err != nil {
				_, msg, _ := errorsx.PublicDetails(err)
				out.Refused = createdBut(out.Key, nv.name+": "+cmpOr(msg, "it could not be set."))
				break
			}
		}
		switch {
		case out.Refused != "":
		case p.refused != "":
			out.Refused = createdBut(out.Key, p.refused)
		default:
			answer, w := s.apply(wctx, caller, p)
			written.Applied = append(written.Applied, w.Applied...)
			switch {
			case answer.Refused != "":
				out.Refused = createdBut(out.Key, answer.Refused)
			case answer.Unknown != "":
				out.Unknown = createdBut(out.Key, answer.Unknown)
			}
		}
	}
	s.h.Wrote(caller.Thread, WriteRow{ID: id, Created: true, Tickets: []WrittenTicket{written}})
	return out, nil
}

// createdBut is a step's sentence after its ticket, key, was created.
func createdBut(key, sentence string) string {
	label, rest, ok := strings.Cut(sentence, ": ")
	if !ok {
		return key + " was created, but " + sentence
	}
	return fmt.Sprintf("%s was created, but %s was not set: %s", key, label, rest)
}

func createUnknown(kind string) error {
	return errorsx.Public(CodeUnknown, trackerNameOf(kind)+"'s answer never came, so the ticket may or may not have been created; the same call again creates it only if it was not.", nil)
}

// createRefused is a create the tracker made nothing of, as the tool's
// refusal: the fields it needs, or the refusal's sentence.
func createRefused(tg createTarget, v trackerapp.WriteView) error {
	for _, p := range v.Parts {
		r := p.Refusal
		switch {
		case r == nil && p.Failure != nil:
			return errorsx.Public(CodeInvalidRequest, "The ticket was not created: "+p.Failure.Message, nil)
		case r == nil:
		case r.Reason == "missing" && len(r.Fields) > 0:
			names := make([]string, len(r.Fields))
			for i, f := range r.Fields {
				names[i] = name(strings.TrimSuffix(f.Message, " is required"))
			}
			return invalidf("%s needs %s to create a ticket; give them in set.", tg.key, sayList(names, "and"))
		default:
			return invalidf("The ticket was not created: %s", refusalText(r))
		}
	}
	return invalidf("The ticket was not created; the tracker gave no reason.")
}

// createTarget is the connection and board a create in project (KEY or
// site/KEY) goes through: the one site that has a board of the project
// here (Host.CreateBoard).
func (s *Server) createTarget(threadID, project string) (createTarget, error) {
	project = strings.TrimSpace(project)
	site, key, qualified := strings.Cut(project, "/")
	if !qualified {
		site, key = "", project
	}
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return createTarget{}, invalidf("project is empty; give its key, such as WEB for WEB-1.")
	}
	sources, err := s.sources()
	if err != nil {
		return createTarget{}, err
	}
	asked := sources
	if qualified {
		asked = slices.DeleteFunc(slices.Clone(sources), func(src trackerapp.TicketSource) bool { return !strings.EqualFold(src.Site, strings.TrimSpace(site)) })
		if len(asked) == 0 {
			return createTarget{}, errorsx.Public(CodeNotFound, fmt.Sprintf(
				"No connection on this computer reaches %s; the sites on are %s.", strings.TrimSpace(site), siteList(sources)), nil)
		}
	}
	var hits []createTarget
	for _, src := range asked {
		if slices.ContainsFunc(hits, func(h createTarget) bool { return h.src.Site == src.Site }) {
			continue
		}
		board, ok, err := s.h.CreateBoard(threadID, src.ID, key)
		if err != nil {
			return createTarget{}, err
		}
		if !ok {
			continue
		}
		summary, err := s.t.BoardSummary(src.ID, board)
		if err != nil {
			return createTarget{}, err
		}
		hits = append(hits, createTarget{src: src, board: summary, boardID: board, key: key, sources: sources})
	}
	switch len(hits) {
	case 0:
		return createTarget{}, errorsx.Public(CodeNotFound, fmt.Sprintf(
			"No board of project %s is on this computer; ask the user to add one in Settings, Issues.", key), nil)
	case 1:
		return hits[0], nil
	}
	sites, keys := make([]string, len(hits)), make([]string, len(hits))
	for i, h := range hits {
		sites[i], keys[i] = h.src.Site, h.src.Site+"/"+key
	}
	return createTarget{}, errorsx.Public(CodeAmbiguous, fmt.Sprintf("%s is a project on %s; write it as %s.", key, sayList(sites, "and"), sayList(keys, "or")), nil)
}

// issueType is the type a create in tg's project takes for given: on
// Linear none, refused when given; on Jira the type of that name, or with
// none given Task when the project has it and else its first type that is
// not a subtask.
func (s *Server) issueType(ctx context.Context, tg createTarget, given string) (string, error) {
	if tg.src.Kind == "linear" {
		if given != "" {
			return "", invalidf("Linear has no issue types; leave out type.")
		}
		return "", nil
	}
	types, err := s.t.IssueTypes(ctx, tg.src.ID, tg.board.Project)
	if err != nil {
		return "", err
	}
	want := given
	if want == "" {
		want = "Task"
	}
	var names []string
	for _, ty := range types {
		if strings.EqualFold(ty.Name, want) {
			return ty.ID, nil
		}
		names = append(names, name(ty.Name))
	}
	if given == "" {
		if i := slices.IndexFunc(types, func(ty trackerapp.IssueTypeView) bool { return ty.Level >= 0 }); i >= 0 {
			return types[i].ID, nil
		}
	}
	if len(names) == 0 {
		return "", invalidf("%s has no issue types the user's account can create.", tg.key)
	}
	return "", invalidf("%s has no issue type %s; it has %s.", tg.key, quoteName(cmpOr(given, want)), sayList(names, "and"))
}

// createFields are set's fields as the create sends them, and the
// entries that are steps after it (status, sprint, estimate).
func (s *Server) createFields(ctx context.Context, tg createTarget, typeID string, set []namedValue) ([]trackerapp.FieldEditInput, []namedValue, error) {
	var fields []trackerapp.FieldEditInput
	var after []namedValue
	var offered []trackerapp.FieldView
	read := false
	for _, nv := range set {
		given := strings.TrimSpace(nv.name)
		lower := strings.ToLower(given)
		switch {
		case lower == setStatus || lower == setSprint || lower == setEstimate:
			after = append(after, nv)
			continue
		case lower == setTitle || lower == setDescription || lower == setType || lower == "project":
			return nil, nil, invalidf("Give %s as its own argument, not in set.", lower)
		case readOnlyName(lower) || lower == setComment:
			return nil, nil, invalidf("%s is not something a create sets.", quoteName(given))
		}
		f := trackerapp.FieldView{}
		if id, ok := standardField(lower); ok {
			f = trackerapp.FieldView{ID: string(id), Name: given, Kind: standardKind(id), Editable: true}
		} else {
			if !read {
				var err error
				if offered, err = s.t.CreateFields(ctx, tg.src.ID, tg.board.Project, typeID); err != nil {
					return nil, nil, err
				}
				read = true
			}
			var ok bool
			if f, ok = fieldMatch(offered, given); !ok {
				return nil, nil, invalidf("A %s ticket has no field %s; it has %s.", tg.key, quoteName(given), fieldNames(offered, createTop))
			}
		}
		edit, err := createEdit(f, given, nv.value, tg.src.Account)
		if err != nil {
			return nil, nil, err
		}
		fields = append(fields, edit)
	}
	return fields, after, nil
}

// createTop are the names a refusal of a create's unknown name lists
// first.
var createTop = []string{setStatus, setSprint, setEstimate, setAssignee, setPriority, setLabels, setParent}

// createEdit is v for field f of a create, which set names given.
func createEdit(f trackerapp.FieldView, given string, v setValue, account string) (trackerapp.FieldEditInput, error) {
	switch {
	case v.null:
		return trackerapp.FieldEditInput{Field: f.ID, Value: &trackerapp.FieldValueView{}}, nil
	case f.Kind == tracker.KindRichText.String():
		md, err := v.one(given)
		if err != nil {
			return trackerapp.FieldEditInput{}, err
		}
		return trackerapp.FieldEditInput{Field: f.ID, Markdown: &trackerapp.MarkdownEditInput{Markdown: md}}, nil
	case f.Kind == tracker.KindUser.String():
		edit := userValue(v, account)
		edit.Field = f.ID
		return edit, nil
	}
	return trackerapp.FieldEditInput{Field: f.ID, Typed: typedOf(f.Kind, tracker.FieldID(f.ID), v)}, nil
}

// repeatCreate checks a create of title against an earlier call's in the
// last hour: one applied is the ticket it made, and one whose answer
// never came is searched for in the project by its title, created near
// the time it was sent. found reports the ticket, by its key as answers
// write it.
func (s *Server) repeatCreate(ctx context.Context, caller Caller, tg createTarget, typeID, title string) (string, bool, error) {
	rep, ok, err := s.t.AgentRepeat(caller.Thread, trackerapp.CreateDigest(tg.src.ID, tg.board.Project, typeID, title))
	if err != nil || !ok {
		return "", false, err
	}
	issue := rep.Issue
	if rep.State != "applied" || issue == "" {
		page, err := s.t.Search(ctx, tg.src.ID, trackerapp.SearchRequest{Text: title, Project: tg.board.Project, Limit: pageSize})
		if err != nil {
			return "", false, errorsx.Public(CodeInvalidRequest, "An earlier call may have created this ticket, and the tracker could not be searched to see, so nothing was created; the same call again tries again.", err)
		}
		i := slices.IndexFunc(page.Cards, func(c trackerapp.CardView) bool {
			return c.Title == title && c.Created >= rep.At-repeatBefore && c.Created <= rep.At+repeatAfter
		})
		if i < 0 {
			return "", false, nil
		}
		issue = page.Cards[i].ID
		if err := s.t.SettleAgentRepeat(rep.Row, issue, ""); err != nil {
			s.logf("tracker tools: record the ticket %s found created: %v", issue, err)
		}
	}
	key, err := s.keyOf(ctx, tg, issue)
	return key, err == nil, err
}

// keyOf is issue's key on tg's connection as answers write it: from the
// mirror, else read.
func (s *Server) keyOf(ctx context.Context, tg createTarget, issue string) (string, error) {
	q := qualifier(tg.sources, tg.src.Site)
	if card, ok, err := s.t.MirrorCard(tg.src.ID, issue); err != nil || ok {
		return q(card.Key), err
	}
	d, err := s.t.IssueDetail(ctx, tg.src.ID, issue)
	return q(d.Card.Key), err
}

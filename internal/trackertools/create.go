package trackertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// ticket_create makes one ticket, through a board of its project this
// computer mirrors (createTarget), so the new ticket is read into the
// board as a person's create is. Every field of set but the status, the
// sprint and the estimate is part of the create, which both trackers
// check before anything is made: a value refused there leaves nothing
// created. The status, sprint and estimate are the ticket's own steps
// after it exists (ticket_update's plan and apply). The key comes from
// the create's own answer, so a confirmed create answers its key and
// writes its row whatever a later read does. The create request is
// recorded before it is sent (trackerapp's repeat record): the same call
// again finds the ticket an earlier one made, sends nothing for it, and
// sets only the steps after it that the ticket does not already hold.

type createArgs struct {
	Project     string                     `json:"project"`
	Title       string                     `json:"title"`
	Type        string                     `json:"type"`
	Description string                     `json:"description"`
	Set         map[string]json.RawMessage `json:"set"`
}

// createAnswer is what became of a create: the ticket's key, and of the
// steps after it, those applied, those the ticket already held, the one
// refused or unknown with its sentence, and those not sent after it.
type createAnswer struct {
	Key     string   `json:"key"`
	Applied []string `json:"applied,omitempty"`
	Already []string `json:"already,omitempty"`
	Refused string   `json:"refused,omitempty"`
	Unknown string   `json:"unknown,omitempty"`
	NotSent []string `json:"not_sent,omitempty"`
	Note    string   `json:"note,omitempty"`
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
	// The create is admitted now: a thread made read-only while the call
	// read the project sends nothing.
	if err := s.h.Writable(caller.Thread); err != nil {
		return nil, err
	}
	id := s.newID()
	wctx := trackerapp.ByAgent(context.WithoutCancel(ctx), caller.Thread, id)
	v, err := s.t.Create(wctx, tg.src.ID, trackerapp.CreateRequest{Board: tg.boardID, Project: tg.board.Project, Type: typeID, Title: title, Fields: fields})
	switch {
	case errors.Is(err, trackerapp.ErrAgentWriteInFlight):
		return nil, invalidf("The same ticket is still being created by an earlier call, so it was not created again; call again once that call has answered.")
	case errors.Is(err, trackerapp.ErrAgentRepeatUnchecked):
		return nil, errorsx.Public(CodeUnknown, uncheckedSentence("An earlier call may have created this ticket", "the tracker could not be searched to check", "created", "find the ticket by its title to see whether it is there", err), err)
	case err != nil:
		if _, msg, ok := errorsx.PublicDetails(err); ok && strings.Contains(msg, "may or may not") {
			return nil, createUnknown(tg.src.Kind)
		}
		return nil, err
	}
	q := qualifier(tg.sources, tg.src.Site)
	out := createAnswer{}
	written := WrittenTicket{Connection: tg.src.ID, Title: title}
	var issue, key string
	if v.Repeat != nil {
		issue, key, out.Note = v.Repeat.Issue, v.Repeat.Key, "Your earlier call created it; it was not created again."
	} else {
		if v.CreateUnknown || v.Created == "" && slices.ContainsFunc(v.Parts, func(p trackerapp.PartView) bool { return p.State == "unknown" }) {
			return nil, createUnknown(tg.src.Kind)
		}
		if v.Created == "" {
			return nil, createRefused(tg, v)
		}
		issue, key = v.Created, v.CreatedKey
		written.Applied = []string{"created"}
		if v.NotOnBoard != "" {
			out.Note = fmt.Sprintf("Board %s does not show it: its filter leaves it out.", quoteName(tg.board.Name))
		}
	}
	out.Key = q(key)
	written.Issue, written.Key = issue, out.Key
	// A part after the create (an image the description embeds) is the
	// created ticket's.
	var stopped bool
	if state, sentence := judge(tg.src.Kind, setDescription, "change", trackerapp.WriteView{Parts: slices.DeleteFunc(slices.Clone(v.Parts),
		func(p trackerapp.PartView) bool { return p.Step == "create" })}, nil, nil); state != tracker.StateApplied {
		out.Refused, stopped = sentence, true
	}
	if len(after) > 0 {
		s.createSteps(wctx, caller, tg, ticket{connection: tg.src.ID, site: tg.src.Site, issue: issue, key: key}, after, stopped, &out, &written)
	}
	if len(written.Applied) > 0 {
		s.h.Wrote(caller.Thread, WriteRow{ID: id, Created: v.Repeat == nil, Tickets: []WrittenTicket{written}})
	}
	return out, nil
}

// createSteps plans and sends the steps after the create, after, on t,
// the ticket made; stopped reports a part of the create refused, which
// sends none of them.
func (s *Server) createSteps(wctx context.Context, caller Caller, tg createTarget, t ticket, after []namedValue, stopped bool, out *createAnswer, written *WrittenTicket) {
	names := func(from int) []string {
		var ns []string
		for _, nv := range after[from:] {
			ns = append(ns, strings.TrimSpace(nv.name))
		}
		return ns
	}
	if stopped {
		out.NotSent = names(0)
		return
	}
	// On wctx, as the steps are: a caller gone after the create was sent
	// still has the steps after it sent. The read and the steps share one
	// write session, as an update's do.
	wctx = trackerapp.WithWrites(wctx)
	pc := newPlanCall(s, caller, tg.sources)
	plans, errs, err := pc.readTickets(wctx, []ticket{t}, after, "")
	if err == nil {
		err = errs[0]
	}
	if err != nil {
		s.logf("tracker tools: read %s after its create: %v", t.key, err)
		out.NotSent = names(0)
		out.Note = strings.TrimSpace(out.Note + " " + out.Key + " could not be read to set " + sayList(out.NotSent, "and") + "; ticket_update sets them.")
		return
	}
	p := plans[0]
	p.key = out.Key
	for i, nv := range after {
		if err := pc.planStep(p, nv); err != nil {
			_, msg, _ := errorsx.PublicDetails(err)
			// Nothing is sent: every step but the one refused is not sent.
			out.Refused, out.NotSent = strings.TrimSpace(nv.name)+": "+cmpOr(msg, "it could not be set."), slices.Delete(names(0), i, i+1)
			return
		}
	}
	if p.refused != "" {
		out.Refused, out.NotSent = p.refused, p.stepNames(true, p.refusedStep)
		return
	}
	answers, w := s.apply(wctx, caller, []*planned{p})
	answer := answers[0]
	out.Applied, out.Already, out.NotSent = answer.Applied, answer.Already, answer.NotSent
	out.Refused, out.Unknown = answer.Refused, answer.Unknown
	written.Applied = append(written.Applied, w[0].Applied...)
}

func createUnknown(kind string) error {
	return errorsx.Public(CodeUnknown, trackerNameOf(kind)+"'s answer never came, so the ticket may or may not have been created; the same call again looks for it first.", nil)
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
				names[i] = quoteName(strings.TrimSuffix(f.Message, " is required"))
			}
			return invalidf("%s needs %s to create a ticket; give them in set.", tg.key, sayList(names, "and"))
		default:
			return invalidf("The ticket was not created: %s", refusalText(tg.src.Kind, r, true, nil))
		}
	}
	return invalidf("The ticket was not created; the tracker gave no reason.")
}

// createTarget is the connection and board a create in project (KEY or
// site/KEY) goes through, chosen once over every board this computer
// lists of that project (Host.Boards) on the connections asked: a
// board the mirror holds that the thread's project lists, else any board
// the mirror holds. A project whose chosen boards are on two sites is
// refused as ambiguous; one whose boards the mirror holds none of is
// refused as not read yet.
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
	_, boards, err := s.h.Boards(threadID)
	if err != nil {
		return createTarget{}, err
	}
	var thread, any []createTarget
	unread := ""
	for _, b := range boards {
		if !strings.EqualFold(b.TrackerProject, key) {
			continue
		}
		i := slices.IndexFunc(asked, func(src trackerapp.TicketSource) bool { return src.ID == b.Connection })
		if i < 0 {
			continue
		}
		summary, err := s.t.BoardSummary(b.Connection, b.Board)
		if errors.Is(err, trackerapp.ErrNotMirrored) {
			// The board is listed, but no screen has shown it since it was,
			// so its project and sprints are not here yet.
			unread = cmpOr(unread, asked[i].Site)
			continue
		}
		if err != nil {
			return createTarget{}, err
		}
		tg := createTarget{src: asked[i], board: summary, boardID: b.Board, key: key, sources: sources}
		if b.Thread {
			thread = append(thread, tg)
		}
		any = append(any, tg)
	}
	hits := thread
	if len(hits) == 0 {
		hits = any
	}
	var sites []string
	for _, h := range hits {
		if !slices.Contains(sites, h.src.Site) {
			sites = append(sites, h.src.Site)
		}
	}
	switch {
	case len(sites) == 1:
		return hits[0], nil
	case len(sites) > 1:
		keys := make([]string, len(sites))
		for i, site := range sites {
			keys[i] = site + "/" + key
		}
		return createTarget{}, errorsx.Public(CodeAmbiguous, fmt.Sprintf("%s is a project on %s; write it as %s.", key, sayList(sites, "and"), sayList(keys, "or")), nil)
	case unread != "":
		return createTarget{}, errorsx.Public(CodeNotFound, fmt.Sprintf(
			"Project %s's board on %s has not been read on this computer yet; ask the user to open it in Issues, then call again.", key, unread), nil)
	}
	return createTarget{}, errorsx.Public(CodeNotFound, fmt.Sprintf(
		"No board of project %s is on this computer; ask the user to add one in Settings, Issues.", key), nil)
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
		names = append(names, quoteName(ty.Name))
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
		case lower == setTitle || lower == setDescription || lower == setType || lower == "project" && tg.src.Kind != "linear":
			return nil, nil, invalidf("Give %s as its own argument, not in set.", lower)
		case readOnlyName(lower) || lower == setComment:
			return nil, nil, invalidf("%s is not something a create sets.", quoteName(given))
		}
		var f trackerapp.FieldView
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
		edit, err := createEdit(f, given, nv.value, tg)
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

// createEdit is v for field f of a create through tg, which set names
// given.
func createEdit(f trackerapp.FieldView, given string, v setValue, tg createTarget) (trackerapp.FieldEditInput, error) {
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
		edit := userValue(v, tg.src.Account)
		edit.Field = f.ID
		return edit, nil
	case f.ID == string(tracker.FieldParent):
		typed, err := parentKeys(v, tg.src.Kind, tg.src.Site, "project "+tg.key)
		if err != nil {
			return trackerapp.FieldEditInput{}, err
		}
		return trackerapp.FieldEditInput{Field: f.ID, Typed: typed}, nil
	}
	return trackerapp.FieldEditInput{Field: f.ID, Typed: typedOf(f.Kind, v)}, nil
}

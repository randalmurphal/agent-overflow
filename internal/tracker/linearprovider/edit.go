package linearprovider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// changes is a create's or an edit's fields as Linear's values.
type changes struct {
	title       linear.Optional[string]
	description linear.Optional[string]
	assignee    linear.Optional[string]
	priority    linear.Optional[linear.Priority]
	labels      linear.Optional[[]string]
	parent      linear.Optional[string]
	project     linear.Optional[string]
	dueDate     linear.Optional[linear.Date]
	// descriptionFrom is the Markdown a description edit was written
	// from; nil when the description is not edited as Markdown.
	descriptionFrom *string
}

// resolver turns field edits into changes for one team, reading a
// catalog the first time a typed value needs it.
type resolver struct {
	p        *Provider
	team     string
	members  []linear.User
	labels   []linear.Label
	projects []linear.Project
	read     map[tracker.FieldID]bool
}

// refusedEdit is an edit refused before any request.
type refusedEdit struct{ tracker.Refusal }

func (r *refusedEdit) Error() string { return r.Message }

func refusedf(reason tracker.Reason, format string, args ...any) *refusedEdit {
	msg := fmt.Sprintf(format, args...)
	return &refusedEdit{tracker.Refusal{Reason: reason, Message: msg, Err: fmt.Errorf("linearprovider: %s", msg)}}
}

// needsTeam reports whether resolving edits reads the team's catalogs.
func needsTeam(edits []tracker.FieldEdit) bool {
	for _, e := range edits {
		if e.Value.Form() == tracker.EditTyped && (e.Field == tracker.FieldAssignee || e.Field == tracker.FieldLabels || e.Field == FieldProject) {
			return true
		}
	}
	return false
}

// resolve resolves edits. A refusal is a *refusedEdit; any other error
// is a read that failed. A clear is kept as a clear on a create too:
// changes.create decides what Linear needs to hear of it.
func (r *resolver) resolve(ctx context.Context, edits []tracker.FieldEdit, create bool) (changes, error) {
	var ch changes
	for _, e := range edits {
		f, ok := spec(e.Field)
		if !ok {
			return changes{}, refusedf(tracker.ReasonUnsupported, "Linear has no field %q", e.Field)
		}
		var err error
		switch e.Value.Form() {
		case tracker.EditClear:
			err = clearField(f, &ch)
		case tracker.EditPick:
			err = pick(f, e.Value.Values(), &ch)
		case tracker.EditTyped:
			err = r.typed(ctx, f, e.Value.Values(), &ch)
		case tracker.EditMarkdown:
			err = markdown(f, e.Value, create, &ch)
		default:
			err = refusedf(tracker.ReasonInvalid, "the edit of %s gives no value", f.id)
		}
		if err != nil {
			return changes{}, err
		}
	}
	return ch, nil
}

func clearField(f fieldSpec, ch *changes) error {
	switch f.id {
	case tracker.FieldTitle:
		return refusedf(tracker.ReasonInvalid, "the title cannot be cleared")
	case tracker.FieldDescription:
		ch.description = linear.Null[string]()
	case tracker.FieldAssignee:
		ch.assignee = linear.Null[string]()
	case tracker.FieldPriority:
		ch.priority = linear.Set(linear.PriorityNone)
	case tracker.FieldLabels:
		ch.labels = linear.Null[[]string]()
	case tracker.FieldParent:
		ch.parent = linear.Null[string]()
	case FieldProject:
		ch.project = linear.Null[string]()
	case FieldDueDate:
		ch.dueDate = linear.Null[linear.Date]()
	}
	return nil
}

func pick(f fieldSpec, ids []string, ch *changes) error {
	if f.kind != tracker.KindUser && f.kind != tracker.KindOption {
		return refusedf(tracker.ReasonInvalid, "%s takes no picked value", f.name)
	}
	if !f.list && len(ids) != 1 {
		return refusedf(tracker.ReasonInvalid, "%s takes one value, not %d", f.name, len(ids))
	}
	for _, id := range ids {
		if id == "" {
			return refusedf(tracker.ReasonInvalid, "%s is picked with an empty ID", f.name)
		}
	}
	switch f.id {
	case tracker.FieldAssignee:
		ch.assignee = linear.Set(ids[0])
	case tracker.FieldPriority:
		v, err := strconv.Atoi(ids[0])
		if err != nil || v < 0 || v >= len(priorityNames) {
			return refusedf(tracker.ReasonInvalid, "%q is not a priority", ids[0])
		}
		ch.priority = linear.Set(linear.Priority(v))
	case tracker.FieldLabels:
		ch.labels = linear.Set(ids)
	case tracker.FieldParent:
		ch.parent = linear.Set(ids[0])
	case FieldProject:
		ch.project = linear.Set(ids[0])
	}
	return nil
}

func markdown(f fieldSpec, e tracker.Edit, create bool, ch *changes) error {
	if f.id != tracker.FieldDescription {
		return refusedf(tracker.ReasonInvalid, "%s is not rich text", f.name)
	}
	md, from := e.Markdown()
	if md == "" {
		ch.description = linear.Null[string]()
	} else {
		ch.description = linear.Set(md)
	}
	if !create {
		ch.descriptionFrom = &from
	}
	return nil
}

// typed resolves text a person or an agent typed: an assignee by exact
// name, display name or email, priorities and labels and projects by
// exact name (case ignored), a parent by its ID or identifier as given,
// a due date as YYYY-MM-DD.
func (r *resolver) typed(ctx context.Context, f fieldSpec, texts []string, ch *changes) error {
	if !f.list && len(texts) != 1 {
		return refusedf(tracker.ReasonInvalid, "%s takes one value, not %d", f.name, len(texts))
	}
	text := strings.TrimSpace(texts[0])
	switch f.id {
	case tracker.FieldTitle:
		if text == "" {
			return refusedf(tracker.ReasonInvalid, "the title cannot be empty")
		}
		ch.title = linear.Set(texts[0])
	case tracker.FieldDescription:
		return refusedf(tracker.ReasonInvalid, "the description is rich text: edit it as Markdown")
	case tracker.FieldParent:
		if text == "" {
			return refusedf(tracker.ReasonNoMatch, "an empty parent names no issue")
		}
		ch.parent = linear.Set(text)
	case FieldDueDate:
		d, err := tracker.ParseDate(text)
		if err != nil {
			return refusedf(tracker.ReasonNoMatch, "%q is not a date as YYYY-MM-DD", text)
		}
		ch.dueDate = linear.Set(linear.Date(d.String()))
	case tracker.FieldPriority:
		var all []candidate
		for v, name := range priorityNames {
			all = append(all, candidate{tracker.Choice{ID: strconv.Itoa(v), Name: name}, []string{name}})
		}
		id, err := matchOne(f, text, all)
		if err != nil {
			return err
		}
		v, _ := strconv.Atoi(id)
		ch.priority = linear.Set(linear.Priority(v))
	default:
		all, err := r.candidates(ctx, f.id)
		if err != nil {
			return err
		}
		var ids []string
		for _, t := range texts {
			id, err := matchOne(f, strings.TrimSpace(t), all)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		switch f.id {
		case tracker.FieldAssignee:
			ch.assignee = linear.Set(ids[0])
		case tracker.FieldLabels:
			ch.labels = linear.Set(ids)
		case FieldProject:
			ch.project = linear.Set(ids[0])
		}
	}
	return nil
}

// candidate is a value typed text can name, by any of its keys.
type candidate struct {
	choice tracker.Choice
	keys   []string
}

// candidates are the values of an assignee, labels or project field in
// the resolver's team, read once.
func (r *resolver) candidates(ctx context.Context, field tracker.FieldID) ([]candidate, error) {
	if r.read == nil {
		r.read = map[tracker.FieldID]bool{}
	}
	var err error
	if !r.read[field] {
		switch field {
		case tracker.FieldAssignee:
			r.members, err = r.p.assignable(ctx, r.team)
		case tracker.FieldLabels:
			r.labels, err = r.p.applicableLabels(ctx, r.team)
		case FieldProject:
			r.projects, err = r.p.teamProjects(ctx, r.team)
		}
		if err != nil {
			return nil, err
		}
		r.read[field] = true
	}
	var out []candidate
	switch field {
	case tracker.FieldAssignee:
		for _, u := range r.members {
			out = append(out, candidate{tracker.Choice{ID: u.ID, Name: userName(u)}, []string{u.Name, u.DisplayName, u.Email}})
		}
	case tracker.FieldLabels:
		for _, l := range r.labels {
			out = append(out, candidate{tracker.Choice{ID: l.ID, Name: l.Name}, []string{l.Name}})
		}
	case FieldProject:
		for _, pr := range r.projects {
			out = append(out, candidate{tracker.Choice{ID: pr.ID, Name: pr.Name}, []string{pr.Name}})
		}
	}
	return out, nil
}

// maxCandidates bounds the suggestions a no-match refusal carries.
const maxCandidates = 20

// matchOne is the ID of the one candidate text names exactly, case
// ignored. No match is refused with the candidates whose keys start with
// text (or, when none does, the first ones); several are refused as
// ambiguous with all of them.
func matchOne(f fieldSpec, text string, all []candidate) (string, error) {
	var exact, prefix []tracker.Choice
	for _, c := range all {
		hit, starts := false, false
		for _, k := range c.keys {
			hit = hit || (k != "" && strings.EqualFold(k, text))
			starts = starts || (k != "" && hasPrefixFold(k, text))
		}
		if hit {
			exact = append(exact, c.choice)
		} else if starts {
			prefix = append(prefix, c.choice)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0].ID, nil
	case len(exact) > 1:
		e := refusedf(tracker.ReasonAmbiguous, "%q names %d values of %s; pick one by ID", text, len(exact), f.name)
		e.Candidates = exact
		return "", e
	}
	if len(prefix) == 0 {
		for _, c := range all {
			prefix = append(prefix, c.choice)
		}
	}
	e := refusedf(tracker.ReasonNoMatch, "%q names no value of %s", text, f.name)
	e.Candidates = prefix[:min(len(prefix), maxCandidates)]
	return "", e
}

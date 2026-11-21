package trackerfake

import (
	"fmt"
	"slices"
	"time"
)

// linearFold is how long after an issue's creation its changes are
// folded into the creation and never become history entries (measured:
// changes at 2m30s were folded, at 3m recorded; the bound in between is
// unverified).
const linearFold = 3 * time.Minute

// linearInputs are the members of each input object type the mutations
// read. The pre-pass refuses any other member, and done fails a request
// whose resolver left a listed member unread.
var linearInputs = map[string][]string{
	"IssueCreateInput":         issueMemberNames(false),
	"IssueUpdateInput":         issueMemberNames(true),
	"CommentCreateInput":       {"issueId", "body", "parentId"},
	"CommentUpdateInput":       {"body"},
	"IssueRelationCreateInput": {"issueId", "relatedIssueId", "type"},
	"AttachmentCreateInput":    {"issueId", "url", "title", "subtitle"},
}

// linearInput reads an input object's members. Reading never changes the
// value, which a variable may share with another field.
type linearInput struct {
	r    *gqlRun
	what string
	m    map[string]any
	read map[string]bool
}

func (r *gqlRun) input(args map[string]any, what string) *linearInput {
	raw, _ := take(args, "input")
	m, _ := raw.(map[string]any)
	return &linearInput{r: r, what: what, m: m, read: map[string]bool{}}
}

// get is a member: present, and its value (nil for an explicit null).
func (in *linearInput) get(key string) (any, bool) {
	v, ok := in.m[key]
	in.read[key] = true
	return v, ok
}

// done fails the request on a member the resolver did not read.
func (in *linearInput) done() {
	for k := range in.m {
		if !in.read[k] {
			in.r.fail("input member %s.%s is not implemented", in.what, k)
		}
	}
}

func inputErr(format string, args ...any) *gqlErr {
	return &gqlErr{code: "INPUT_ERROR", message: fmt.Sprintf(format, args...), userError: true}
}

func idArg(args map[string]any) string {
	v, _ := take(args, "id")
	return str(v)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// issueMember is one member of an issue create or update, in the order
// they apply: the Issue field it sets and how. id marks a member naming
// a record, whose empty ID clears it as a null does; updateOnly marks one
// only IssueUpdateInput has.
type issueMember struct {
	name, field    string
	id, updateOnly bool
	set            func(r *gqlRun, is *LinearIssue, v any) *gqlErr
}

func issueMemberNames(update bool) []string {
	var out []string
	for _, m := range issueMembers {
		if update || !m.updateOnly {
			out = append(out, m.name)
		}
	}
	return out
}

var issueMembers = []issueMember{
	{name: "teamId", field: "team", id: true, set: func(r *gqlRun, is *LinearIssue, v any) *gqlErr {
		t := r.w.team(str(v))
		if t == nil {
			return notFound("Team")
		}
		if t.ID != is.Team {
			is.FormerKeys = append(is.FormerKeys, r.w.identifier(is))
			is.Team, is.Number, is.State = t.ID, r.w.nextNumber(t.ID), t.DefaultState
		}
		return nil
	}},
	{name: "title", field: "title", set: func(_ *gqlRun, is *LinearIssue, v any) *gqlErr { is.Title = str(v); return nil }},
	{name: "description", field: "description", set: func(_ *gqlRun, is *LinearIssue, v any) *gqlErr { is.Description = str(v); return nil }},
	{name: "dueDate", field: "dueDate", set: func(_ *gqlRun, is *LinearIssue, v any) *gqlErr { is.DueDate = str(v); return nil }},
	issueRef("stateId", "state", func(w *linearWorkspace, is *LinearIssue, id string) (*string, bool) {
		s := w.state(id)
		return &is.State, s != nil && s.Team == is.Team
	}),
	issueRef("assigneeId", "assignee", func(w *linearWorkspace, is *LinearIssue, id string) (*string, bool) {
		return &is.Assignee, w.user(id) != nil
	}),
	issueRef("cycleId", "cycle", func(w *linearWorkspace, is *LinearIssue, id string) (*string, bool) {
		c := w.cycle(id)
		return &is.Cycle, c != nil && c.Team == is.Team
	}),
	issueRef("projectId", "project", func(w *linearWorkspace, is *LinearIssue, id string) (*string, bool) {
		return &is.Project, w.project(id) != nil
	}),
	{name: "parentId", field: "parent", id: true, set: func(r *gqlRun, is *LinearIssue, v any) *gqlErr {
		id := str(v)
		if id == "" {
			is.Parent = ""
			return nil
		}
		p := r.w.issue(id)
		if p == nil || p.ID == is.ID { // an issue as its own parent: unverified
			return missingRef("parentId")
		}
		is.Parent = p.ID
		return nil
	}},
	{name: "priority", field: "priority", set: func(_ *gqlRun, is *LinearIssue, v any) *gqlErr {
		p := toFloat(v)
		if p < 0 || p > 4 {
			return inputErr("priority must be between 0 and 4")
		}
		is.Priority = p
		return nil
	}},
	{name: "estimate", field: "estimate", set: func(_ *gqlRun, is *LinearIssue, v any) *gqlErr { is.Estimate = numOrUnset(v); return nil }},
	{name: "subIssueSortOrder", field: "subIssueSortOrder", set: func(_ *gqlRun, is *LinearIssue, v any) *gqlErr {
		is.SubIssueSortOrder = numOrUnset(v)
		return nil
	}},
	{name: "sortOrder", field: "sortOrder", set: func(_ *gqlRun, is *LinearIssue, v any) *gqlErr { is.SortOrder = toFloat(v); return nil }},
	{name: "prioritySortOrder", field: "prioritySortOrder", set: func(_ *gqlRun, is *LinearIssue, v any) *gqlErr {
		is.PrioritySortOrder = toFloat(v)
		return nil
	}},
	{name: "labelIds", field: "labelIds", set: func(r *gqlRun, is *LinearIssue, v any) *gqlErr {
		list, err := r.labelIDs("labelIds", v)
		if err == nil {
			is.Labels = list
		}
		return err
	}},
	{name: "addedLabelIds", field: "labelIds", updateOnly: true, set: func(r *gqlRun, is *LinearIssue, v any) *gqlErr {
		list, err := r.labelIDs("addedLabelIds", v)
		for _, id := range list {
			if !slices.Contains(is.Labels, id) {
				is.Labels = append(is.Labels, id)
			}
		}
		return err
	}},
	{name: "removedLabelIds", field: "labelIds", updateOnly: true, set: func(r *gqlRun, is *LinearIssue, v any) *gqlErr {
		list, err := r.labelIDs("removedLabelIds", v)
		is.Labels = slices.DeleteFunc(is.Labels, func(id string) bool { return slices.Contains(list, id) })
		return err
	}},
}

// issueRef is a member naming a record by ID; target is the issue field
// it sets and whether id names a record the issue may reference. One it
// may not is refused as one that does not exist (missingRef; a state or
// cycle of another team is unverified).
func issueRef(name, field string, target func(w *linearWorkspace, is *LinearIssue, id string) (*string, bool)) issueMember {
	return issueMember{name: name, field: field, id: true, set: func(r *gqlRun, is *LinearIssue, v any) *gqlErr {
		dst, ok := target(r.w, is, str(v))
		if str(v) != "" && !ok {
			return missingRef(name)
		}
		*dst = str(v)
		return nil
	}}
}

func numOrUnset(v any) *float64 {
	if v == nil {
		return nil
	}
	n := toFloat(v)
	return &n
}

// labelIDs reads member's list of label IDs, refusing one the workspace
// lacks.
func (r *gqlRun) labelIDs(member string, v any) ([]string, *gqlErr) {
	list, _ := v.([]any)
	out := []string{}
	for _, x := range list {
		if r.w.label(str(x)) == nil {
			return nil, missingRef(member)
		}
		out = append(out, str(x))
	}
	return out, nil
}

// setIssue applies the members of an issue create or update to is. A
// null, or an empty ID, clears a field the schema lets be null and is
// refused for one it does not (state, team, title, priority, the sort
// orders, the labels): the schema says the issue always has one, and
// Linear's own answer to it is unverified.
func (r *gqlRun) setIssue(in *linearInput, is *LinearIssue) *gqlErr {
	for _, m := range issueMembers {
		v, ok := in.get(m.name)
		if !ok {
			continue
		}
		if (v == nil || m.id && v == "") && r.nonNull("Issue", m.field) {
			return inputErr("%s must not be null", m.name)
		}
		if err := m.set(r, is, v); err != nil {
			return err
		}
	}
	return nil
}

// nonNull reports a field of typ the schema declares non-null.
func (r *gqlRun) nonNull(typ, field string) bool {
	if def := r.s.Types[typ]; def != nil {
		if f := def.Fields.ForName(field); f != nil {
			return f.Type.NonNull
		}
	}
	return false
}

// stamp sets the state's timestamps after a state change: started,
// completed and canceled times; moving back clears the later ones
// (unverified).
func (w *linearWorkspace) stamp(is *LinearIssue, now time.Time) {
	switch w.state(is.State).Type {
	case "started":
		is.StartedAt = orNow(is.StartedAt, now)
		is.CompletedAt, is.CanceledAt = time.Time{}, time.Time{}
	case "completed":
		is.CompletedAt, is.CanceledAt = now, time.Time{}
	case "canceled":
		is.CanceledAt, is.CompletedAt = now, time.Time{}
	default:
		is.StartedAt, is.CompletedAt, is.CanceledAt = time.Time{}, time.Time{}, time.Time{}
	}
}

// record adds the history entry of the change from before to is, by
// actor, unless it falls in the creation's fold window or changes
// nothing Linear records. It reports whether anything changed.
func (w *linearWorkspace) record(e *Engine, before, is *LinearIssue, actor string, now time.Time) bool {
	h := LinearHistory{CreatedAt: now, Actor: actor}
	changed := false
	pair := func(from, to string, f, t *string) {
		if from != to {
			*f, *t, changed = from, to, true
		}
	}
	pair(before.State, is.State, &h.FromState, &h.ToState)
	pair(before.Assignee, is.Assignee, &h.FromAssignee, &h.ToAssignee)
	pair(before.Cycle, is.Cycle, &h.FromCycle, &h.ToCycle)
	pair(before.Project, is.Project, &h.FromProject, &h.ToProject)
	pair(before.Parent, is.Parent, &h.FromParent, &h.ToParent)
	pair(before.Team, is.Team, &h.FromTeam, &h.ToTeam)
	pair(before.Title, is.Title, &h.FromTitle, &h.ToTitle)
	if !sameNum(before.Estimate, is.Estimate) {
		h.FromEstimate, h.ToEstimate, changed = before.Estimate, is.Estimate, true
	}
	if before.Priority != is.Priority {
		from, to := before.Priority, is.Priority
		h.FromPriority, h.ToPriority, changed = &from, &to, true
	}
	for _, id := range is.Labels {
		if !slices.Contains(before.Labels, id) {
			h.AddedLabels, changed = append(h.AddedLabels, id), true
		}
	}
	for _, id := range before.Labels {
		if !slices.Contains(is.Labels, id) {
			h.RemovedLabels, changed = append(h.RemovedLabels, id), true
		}
	}
	if before.ArchivedAt.IsZero() != is.ArchivedAt.IsZero() {
		archived := !is.ArchivedAt.IsZero()
		h.Archived, changed = &archived, true
	}
	other := before.Description != is.Description || before.DueDate != is.DueDate || before.SortOrder != is.SortOrder ||
		before.PrioritySortOrder != is.PrioritySortOrder || !sameNum(before.SubIssueSortOrder, is.SubIssueSortOrder) || before.Trashed != is.Trashed
	if !changed && !other {
		return false
	}
	is.UpdatedAt = now
	if changed && now.Sub(is.CreatedAt) >= linearFold {
		h.ID = linearID(e)
		is.History = append(is.History, h)
	}
	return true
}

func sameNum(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func (m mutationRoot) createIssue(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	in := r.input(args, "IssueCreateInput")
	team, _ := in.get("teamId")
	t := w.team(str(team))
	if t == nil {
		return nil, notFound("Team")
	}
	lowest := 0.0
	for _, other := range w.Issues {
		if other.Team == t.ID {
			lowest = min(lowest, other.SortOrder)
		}
	}
	// Measured: a new issue sorts above every existing one; the step is
	// unverified.
	is := LinearIssue{
		ID: linearID(r.e), Team: t.ID, Number: w.nextNumber(t.ID), State: t.DefaultState, Creator: w.Viewer,
		SortOrder: lowest - 1000, CreatedAt: r.now, UpdatedAt: r.now,
	}
	if err := r.setIssue(in, &is); err != nil {
		return nil, err
	}
	in.done()
	w.stamp(&is, r.now)
	w.Issues = append(w.Issues, is)
	return issueObj{w: w, is: &w.Issues[len(w.Issues)-1]}, nil
}

// foundIssue is the issue an id argument names, or Linear's not-found.
func (w *linearWorkspace) foundIssue(args map[string]any) (*LinearIssue, *gqlErr) {
	if is := w.issue(idArg(args)); is != nil {
		return is, nil
	}
	return nil, notFound("Issue")
}

func (m mutationRoot) updateIssue(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	is, err := w.foundIssue(args)
	if err != nil {
		return nil, err
	}
	in := r.input(args, "IssueUpdateInput")
	before := *is
	before.Labels = slices.Clone(is.Labels)
	if err := r.setIssue(in, is); err != nil {
		return nil, err
	}
	in.done()
	if is.State != before.State {
		w.stamp(is, r.now)
	}
	w.record(r.e, &before, is, w.Viewer, r.now)
	return issueObj{w: w, is: is}, nil
}

// linearMaxBatch is the most issues issueBatchUpdate takes (measured: 51
// IDs answer INVALID_INPUT).
const linearMaxBatch = 50

// batchUpdate applies one IssueUpdateInput to every issue ids names and
// answers with them (measured). A missing ID fails the whole batch with
// INPUT_ERROR "id not found" and changes none (measured; argField's
// transaction puts back the issues already changed). Three issues got
// three distinct update times (measured); here each issue after the
// first is stamped a millisecond after the one before (unverified), and
// the engine's clock moves past the last stamp so it never goes back.
func (m mutationRoot) batchUpdate(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	raw, _ := take(args, "ids")
	ids, _ := raw.([]any)
	if len(ids) > linearMaxBatch {
		return nil, &gqlErr{code: "INVALID_INPUT", message: "Argument Validation Error",
			presentable: fmt.Sprintf("ids must contain at most %d items", linearMaxBatch), userError: true}
	}
	if len(ids) == 0 {
		return nil, inputErr("No issue IDs given") // unverified
	}
	in := r.input(args, "IssueUpdateInput")
	out := make([]gqlObject, 0, len(ids))
	at := r.now
	for i, v := range ids {
		is := w.issue(str(v))
		if is == nil {
			return nil, inputErr("id not found")
		}
		before := *is
		before.Labels = slices.Clone(is.Labels)
		if err := r.setIssue(in, is); err != nil {
			return nil, err
		}
		at = r.now.Add(time.Duration(i) * time.Millisecond)
		if is.State != before.State {
			w.stamp(is, at)
		}
		w.record(r.e, &before, is, w.Viewer, at)
		out = append(out, issueObj{w: w, is: is})
	}
	in.done()
	r.e.last = maxTime(r.e.last, at)
	return out, nil
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (m mutationRoot) archiveIssue(r *gqlRun, args map[string]any) (any, *gqlErr) {
	return m.setArchived(r, args, true)
}

func (m mutationRoot) unarchiveIssue(r *gqlRun, args map[string]any) (any, *gqlErr) {
	return m.setArchived(r, args, false)
}

func (m mutationRoot) setArchived(r *gqlRun, args map[string]any, archived bool) (any, *gqlErr) {
	is, err := m.w.foundIssue(args)
	if err != nil {
		return nil, err
	}
	before := *is
	is.ArchivedAt, is.Trashed = time.Time{}, false
	if archived {
		is.ArchivedAt = r.now
	}
	m.w.record(r.e, &before, is, m.w.Viewer, r.now)
	return issueObj{w: m.w, is: is}, nil
}

// deleteIssue trashes an issue, or removes it and what hangs off it
// when permanentlyDelete is set. The payload's entity is the trashed
// issue (measured, also for an issue already in the trash); a permanent
// delete answers no entity (unverified).
func (m mutationRoot) deleteIssue(r *gqlRun, args map[string]any) (any, *gqlErr) {
	w := m.w
	is, err := w.foundIssue(args)
	if err != nil {
		return nil, err
	}
	if permanently, _ := take(args, "permanentlyDelete"); permanently == true {
		id := is.ID
		w.Issues = slices.DeleteFunc(w.Issues, func(x LinearIssue) bool { return x.ID == id })
		w.Comments = slices.DeleteFunc(w.Comments, func(c LinearComment) bool { return c.Issue == id })
		w.Attachments = slices.DeleteFunc(w.Attachments, func(a LinearAttachment) bool { return a.Issue == id })
		w.Relations = slices.DeleteFunc(w.Relations, func(x LinearRelation) bool { return x.Issue == id || x.Related == id })
		return nil, nil
	}
	before := *is
	is.Trashed, is.ArchivedAt = true, r.now
	w.record(r.e, &before, is, w.Viewer, r.now)
	return issueObj{w: w, is: is}, nil
}

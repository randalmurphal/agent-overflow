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

// linearInput reads an input object's members, each consumed; done
// fails the request on any member left.
type linearInput struct {
	r    *gqlRun
	what string
	m    map[string]any
}

func (r *gqlRun) input(args map[string]any, what string) *linearInput {
	raw, _ := take(args, "input")
	m, _ := raw.(map[string]any)
	return &linearInput{r: r, what: what, m: m}
}

// get is a member: present, and its value (nil for an explicit null).
func (in *linearInput) get(key string) (any, bool) {
	v, ok := in.m[key]
	delete(in.m, key)
	return v, ok
}

func (in *linearInput) done() {
	for k := range in.m {
		in.r.fail("input member %s.%s is not implemented", in.what, k)
	}
}

func inputErr(format string, args ...any) *gqlErr {
	return &gqlErr{code: "INPUT_ERROR", message: fmt.Sprintf(format, args...), userError: true}
}

func idArg(args map[string]any) string {
	v, _ := take(args, "id")
	s, _ := v.(string)
	return s
}

// setIssue applies the members of an issue create or update to is,
// returning Linear's refusal of a reference the workspace lacks.
func (r *gqlRun) setIssue(in *linearInput, is *LinearIssue) *gqlErr {
	w := r.w
	if v, ok := in.get("teamId"); ok {
		t := w.team(str(v))
		if t == nil {
			return notFound("Team")
		}
		if t.ID != is.Team {
			is.Team, is.Number, is.State = t.ID, w.nextNumber(t.ID), t.DefaultState
		}
	}
	for key, dst := range map[string]*string{"title": &is.Title, "description": &is.Description, "dueDate": &is.DueDate} {
		if v, ok := in.get(key); ok {
			*dst = str(v)
		}
	}
	refs := []struct {
		key, typ string
		dst      *string
		exists   func(string) bool
	}{
		{"stateId", "WorkflowState", &is.State, func(id string) bool { s := w.state(id); return s != nil && s.Team == is.Team }},
		{"assigneeId", "User", &is.Assignee, func(id string) bool { return w.user(id) != nil }},
		{"cycleId", "Cycle", &is.Cycle, func(id string) bool { c := w.cycle(id); return c != nil && c.Team == is.Team }},
		{"projectId", "Project", &is.Project, func(id string) bool { return w.project(id) != nil }},
		{"parentId", "Issue", &is.Parent, func(id string) bool { p := w.issue(id); return p != nil && p.ID != is.ID }},
	}
	for _, ref := range refs {
		v, ok := in.get(ref.key)
		if !ok {
			continue
		}
		id := str(v)
		if id != "" && !ref.exists(id) {
			return notFound(ref.typ)
		}
		if ref.key == "parentId" && id != "" {
			id = w.issue(id).ID
		}
		*ref.dst = id
	}
	if v, ok := in.get("priority"); ok {
		p := toFloat(v)
		if p < 0 || p > 4 {
			return inputErr("priority must be between 0 and 4")
		}
		is.Priority = p
	}
	for key, dst := range map[string]**float64{"estimate": &is.Estimate, "subIssueSortOrder": &is.SubIssueSortOrder} {
		if v, ok := in.get(key); ok {
			*dst = nil
			if v != nil {
				n := toFloat(v)
				*dst = &n
			}
		}
	}
	for key, dst := range map[string]*float64{"sortOrder": &is.SortOrder, "prioritySortOrder": &is.PrioritySortOrder} {
		if v, ok := in.get(key); ok {
			*dst = toFloat(v)
		}
	}
	return r.setLabels(in, is)
}

func (r *gqlRun) setLabels(in *linearInput, is *LinearIssue) *gqlErr {
	ids := func(v any) ([]string, *gqlErr) {
		list, _ := v.([]any)
		out := []string{}
		for _, x := range list {
			if r.w.label(str(x)) == nil {
				return nil, notFound("IssueLabel")
			}
			out = append(out, str(x))
		}
		return out, nil
	}
	if v, ok := in.get("labelIds"); ok {
		list, err := ids(v)
		if err != nil {
			return err
		}
		is.Labels = list
	}
	if v, ok := in.get("addedLabelIds"); ok {
		list, err := ids(v)
		if err != nil {
			return err
		}
		for _, id := range list {
			if !slices.Contains(is.Labels, id) {
				is.Labels = append(is.Labels, id)
			}
		}
	}
	if v, ok := in.get("removedLabelIds"); ok {
		list, err := ids(v)
		if err != nil {
			return err
		}
		is.Labels = slices.DeleteFunc(is.Labels, func(id string) bool { return slices.Contains(list, id) })
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
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
	return payloadObj{"IssuePayload", "issue", issueObj{w: w, is: &w.Issues[len(w.Issues)-1]}}, nil
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
	next := *is
	next.Labels = slices.Clone(is.Labels)
	if err := r.setIssue(in, &next); err != nil {
		return nil, err
	}
	in.done()
	if next.State != is.State {
		w.stamp(&next, r.now)
	}
	before := *is
	*is = next
	w.record(r.e, &before, is, w.Viewer, r.now)
	return payloadObj{"IssuePayload", "issue", issueObj{w: w, is: is}}, nil
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
	return payloadObj{"IssueArchivePayload", "entity", issueObj{w: m.w, is: is}}, nil
}

// deleteIssue trashes an issue, or removes it and what hangs off it
// when permanentlyDelete is set.
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
		return payloadObj{typ: "IssueArchivePayload"}, nil
	}
	before := *is
	is.Trashed, is.ArchivedAt = true, r.now
	w.record(r.e, &before, is, w.Viewer, r.now)
	return payloadObj{typ: "IssueArchivePayload"}, nil
}

package trackerfake

import (
	"strings"
	"time"
)

// The objects the executor reads, one per Linear type the clients
// select. A field missing from a value switch is not implemented.

type userObj struct {
	w *linearWorkspace
	u *LinearUser
}

func (userObj) typeName() string { return "User" }
func (o userObj) disabled() bool { return o.u.Disabled }

func (o userObj) value(name string) (any, bool) {
	u := o.u
	switch name {
	case "id":
		return u.ID, true
	case "name":
		return u.Name, true
	case "displayName":
		return orString(u.DisplayName, u.Name), true
	case "email":
		return u.Email, true
	case "avatarUrl":
		return "https://avatars.example.com/" + u.ID + ".png", true
	case "active":
		return !u.Disabled, true
	case "admin":
		return u.Admin, true
	case "guest":
		return u.Guest, true
	case "app":
		return u.App, true
	case "isMe":
		return u.ID == o.w.Viewer, true
	}
	return nil, false
}

// ref is the object of a referenced ID, or nil for none.
func (w *linearWorkspace) userRef(id string) any {
	if u := w.user(id); u != nil {
		return userObj{w, u}
	}
	return nil
}

func (w *linearWorkspace) teamRef(id string) any {
	if t := w.team(id); t != nil {
		return teamObj{w, t}
	}
	return nil
}

func (w *linearWorkspace) stateRef(id string) any {
	if s := w.state(id); s != nil {
		return stateObj{w, s}
	}
	return nil
}

func (w *linearWorkspace) cycleRef(id string) any {
	if c := w.cycle(id); c != nil {
		return cycleObj{w, c}
	}
	return nil
}

func (w *linearWorkspace) projectRef(id string) any {
	if p := w.project(id); p != nil {
		return projectObj{w, p}
	}
	return nil
}

func (w *linearWorkspace) issueRef(id string) any {
	if is := w.issue(id); is != nil {
		return issueObj{w: w, is: is}
	}
	return nil
}

func (w *linearWorkspace) labelRef(id string) any {
	if l := w.label(id); l != nil {
		return labelObj{w, l}
	}
	return nil
}

type orgObj struct{ w *linearWorkspace }

func (orgObj) typeName() string { return "Organization" }

func (o orgObj) value(name string) (any, bool) {
	switch name {
	case "id":
		return o.w.ID, true
	case "name":
		return o.w.Name, true
	case "urlKey":
		return o.w.URLKey, true
	}
	return nil, false
}

type teamObj struct {
	w *linearWorkspace
	t *LinearTeam
}

func (teamObj) typeName() string { return "Team" }
func (o teamObj) archived() bool { return !o.t.ArchivedAt.IsZero() }

func (o teamObj) value(name string) (any, bool) {
	t := o.t
	switch name {
	case "id":
		return t.ID, true
	case "key":
		return t.Key, true
	case "name":
		return t.Name, true
	case "description":
		return stringOrNil(t.Description), true
	case "timezone":
		return t.Timezone, true
	case "issueEstimationType":
		return t.Estimation, true
	case "issueEstimationExtended":
		return t.EstimationExtended, true
	case "issueEstimationAllowZero":
		return t.EstimationZero, true
	case "defaultIssueEstimate":
		return t.DefaultEstimate, true
	case "cyclesEnabled":
		return t.CyclesEnabled, true
	case "cycleDuration":
		return t.CycleWeeks, true
	case "activeCycle":
		for i := range o.w.Cycles {
			if c := (cycleObj{o.w, &o.w.Cycles[i]}); c.c.Team == t.ID && c.active() {
				return c, true
			}
		}
		return nil, true
	case "triageEnabled":
		return t.TriageEnabled, true
	case "defaultIssueState":
		return o.w.stateRef(t.DefaultState), true
	case "triageIssueState":
		return o.w.stateRef(t.TriageState), true
	case "setIssueSortOrderOnStateChange":
		return orString(t.SortOnStateChange, "bottom"), true
	case "archivedAt":
		return t.ArchivedAt, true
	case "updatedAt":
		return t.UpdatedAt, true
	case "members":
		var out []gqlObject
		for _, id := range t.Members {
			out = append(out, userObj{o.w, o.w.user(id)})
		}
		return out, true
	}
	return nil, false
}

type stateObj struct {
	w *linearWorkspace
	s *LinearState
}

func (stateObj) typeName() string { return "WorkflowState" }
func (o stateObj) archived() bool { return !o.s.ArchivedAt.IsZero() }

func (o stateObj) value(name string) (any, bool) {
	s := o.s
	switch name {
	case "id":
		return s.ID, true
	case "name":
		return s.Name, true
	case "color":
		return orString(s.Color, "#bec2c8"), true
	case "description":
		return stringOrNil(s.Description), true
	case "type":
		return s.Type, true
	case "position":
		return s.Position, true
	case "team":
		return o.w.teamRef(s.Team), true
	case "archivedAt":
		return s.ArchivedAt, true
	}
	return nil, false
}

type cycleObj struct {
	w *linearWorkspace
	c *LinearCycle
}

func (cycleObj) typeName() string { return "Cycle" }
func (o cycleObj) archived() bool { return !o.c.ArchivedAt.IsZero() }

func (o cycleObj) now() time.Time { return o.w.clock }

func (o cycleObj) active() bool {
	return o.c.CompletedAt.IsZero() && !o.c.StartsAt.After(o.now()) && o.c.EndsAt.After(o.now())
}
func (o cycleObj) future() bool { return o.c.StartsAt.After(o.now()) }
func (o cycleObj) past() bool   { return !o.c.CompletedAt.IsZero() || !o.c.EndsAt.After(o.now()) }

// neighbor reports the team's next future cycle (next) or its latest past
// one.
func (o cycleObj) neighbor(next bool) bool {
	var best *LinearCycle
	for i := range o.w.Cycles {
		c := cycleObj{o.w, &o.w.Cycles[i]}
		if c.c.Team != o.c.Team || (next && !c.future()) || (!next && !c.past()) {
			continue
		}
		if best == nil || (next && c.c.StartsAt.Before(best.StartsAt)) || (!next && c.c.EndsAt.After(best.EndsAt)) {
			best = c.c
		}
	}
	return best == o.c
}

func (o cycleObj) value(name string) (any, bool) {
	c := o.c
	switch name {
	case "id":
		return c.ID, true
	case "number":
		return c.Number, true
	case "name":
		return stringOrNil(c.Name), true
	case "description":
		return stringOrNil(c.Description), true
	case "team":
		return o.w.teamRef(c.Team), true
	case "startsAt":
		return c.StartsAt, true
	case "endsAt":
		return c.EndsAt, true
	case "completedAt":
		return c.CompletedAt, true
	case "archivedAt":
		return c.ArchivedAt, true
	case "isActive":
		return o.active(), true
	case "isFuture":
		return o.future(), true
	case "isPast":
		return o.past(), true
	case "isNext":
		return o.neighbor(true), true
	case "isPrevious":
		return o.neighbor(false), true
	case "progress":
		return o.progress(), true
	case "scopeHistory":
		return orEmpty(c.ScopeHistory), true
	case "completedScopeHistory":
		return orEmpty(c.CompletedScopeHistory), true
	case "inProgressScopeHistory":
		return orEmpty(c.InProgressScopeHistory), true
	case "issueCountHistory":
		return orEmpty(c.IssueCountHistory), true
	case "completedIssueCountHistory":
		return orEmpty(c.CompletedIssueCountHistory), true
	}
	return nil, false
}

// progress is the share of the cycle's issues completed, by count
// (unverified: Linear weighs it by estimate).
func (o cycleObj) progress() float64 {
	var all, done float64
	for _, is := range o.w.Issues {
		if is.Cycle == o.c.ID {
			all++
			if st := o.w.state(is.State); st.Type == "completed" {
				done++
			}
		}
	}
	if all == 0 {
		return 0
	}
	return done / all
}

type labelObj struct {
	w *linearWorkspace
	l *LinearLabel
}

func (labelObj) typeName() string { return "IssueLabel" }
func (o labelObj) archived() bool { return !o.l.ArchivedAt.IsZero() }

func (o labelObj) value(name string) (any, bool) {
	l := o.l
	switch name {
	case "id":
		return l.ID, true
	case "name":
		return l.Name, true
	case "color":
		return orString(l.Color, "#bec2c8"), true
	case "description":
		return stringOrNil(l.Description), true
	case "isGroup":
		return l.IsGroup, true
	case "parent":
		return o.w.labelRef(l.Parent), true
	case "team":
		return o.w.teamRef(l.Team), true
	case "archivedAt":
		return l.ArchivedAt, true
	}
	return nil, false
}

type projectObj struct {
	w *linearWorkspace
	p *LinearProject
}

func (projectObj) typeName() string { return "Project" }
func (o projectObj) archived() bool { return !o.p.ArchivedAt.IsZero() }

func (o projectObj) value(name string) (any, bool) {
	p := o.p
	switch name {
	case "id":
		return p.ID, true
	case "name":
		return p.Name, true
	case "url":
		return "https://linear.app/" + o.w.URLKey + "/project/" + slug(p.Name) + "-" + p.ID[len(p.ID)-12:], true
	case "status":
		return projectStatusObj{p}, true
	case "lead":
		return o.w.userRef(p.Lead), true
	case "startDate":
		return stringOrNil(p.StartDate), true
	case "targetDate":
		return stringOrNil(p.TargetDate), true
	case "completedAt":
		return p.CompletedAt, true
	case "canceledAt":
		return p.CanceledAt, true
	case "archivedAt":
		return p.ArchivedAt, true
	case "updatedAt":
		return p.UpdatedAt, true
	case "accessibleTeams":
		var out []gqlObject
		for _, id := range p.Teams {
			out = append(out, teamObj{o.w, o.w.team(id)})
		}
		return out, true
	}
	return nil, false
}

type projectStatusObj struct{ p *LinearProject }

func (projectStatusObj) typeName() string { return "ProjectStatus" }

func (o projectStatusObj) value(name string) (any, bool) {
	switch name {
	case "id":
		return o.p.StatusID, true
	case "name":
		return o.p.StatusName, true
	case "type":
		return o.p.StatusType, true
	}
	return nil, false
}

// slug is text as a URL path segment.
func slug(text string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

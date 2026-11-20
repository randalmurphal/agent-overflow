package trackerfake

import (
	"strings"
	"time"
)

// The objects the executor reads, one per Linear type the clients
// select, each answered from its field table (linear_types.go).

type userObj struct {
	w *linearWorkspace
	u *LinearUser
}

func (userObj) typeName() string                { return "User" }
func (o userObj) disabled() bool                { return o.u.Disabled }
func (o userObj) value(name string) (any, bool) { return userFields.get(o, name) }

var userFields = fieldTable[userObj]{
	"id":          func(o userObj) any { return o.u.ID },
	"name":        func(o userObj) any { return o.u.Name },
	"displayName": func(o userObj) any { return orString(o.u.DisplayName, o.u.Name) },
	"email":       func(o userObj) any { return o.u.Email },
	"avatarUrl":   func(o userObj) any { return "https://avatars.example.com/" + o.u.ID + ".png" },
	"active":      func(o userObj) any { return !o.u.Disabled },
	"admin":       func(o userObj) any { return o.u.Admin },
	"guest":       func(o userObj) any { return o.u.Guest },
	"app":         func(o userObj) any { return o.u.App },
	"isMe":        func(o userObj) any { return o.u.ID == o.w.Viewer },
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

func (orgObj) typeName() string                { return "Organization" }
func (o orgObj) value(name string) (any, bool) { return orgFields.get(o, name) }

var orgFields = fieldTable[orgObj]{
	"id":     func(o orgObj) any { return o.w.ID },
	"name":   func(o orgObj) any { return o.w.Name },
	"urlKey": func(o orgObj) any { return o.w.URLKey },
}

type teamObj struct {
	w *linearWorkspace
	t *LinearTeam
}

func (teamObj) typeName() string                { return "Team" }
func (o teamObj) archived() bool                { return !o.t.ArchivedAt.IsZero() }
func (o teamObj) value(name string) (any, bool) { return teamFields.get(o, name) }

var teamFields = fieldTable[teamObj]{
	"id":                       func(o teamObj) any { return o.t.ID },
	"key":                      func(o teamObj) any { return o.t.Key },
	"name":                     func(o teamObj) any { return o.t.Name },
	"description":              func(o teamObj) any { return stringOrNil(o.t.Description) },
	"timezone":                 func(o teamObj) any { return o.t.Timezone },
	"issueEstimationType":      func(o teamObj) any { return o.t.Estimation },
	"issueEstimationExtended":  func(o teamObj) any { return o.t.EstimationExtended },
	"issueEstimationAllowZero": func(o teamObj) any { return o.t.EstimationZero },
	"defaultIssueEstimate":     func(o teamObj) any { return o.t.DefaultEstimate },
	"cyclesEnabled":            func(o teamObj) any { return o.t.CyclesEnabled },
	"cycleDuration":            func(o teamObj) any { return o.t.CycleWeeks },
	"activeCycle": func(o teamObj) any {
		for i := range o.w.Cycles {
			if c := (cycleObj{o.w, &o.w.Cycles[i]}); c.c.Team == o.t.ID && c.active() {
				return c
			}
		}
		return nil
	},
	"triageEnabled":                  func(o teamObj) any { return o.t.TriageEnabled },
	"defaultIssueState":              func(o teamObj) any { return o.w.stateRef(o.t.DefaultState) },
	"triageIssueState":               func(o teamObj) any { return o.w.stateRef(o.t.TriageState) },
	"setIssueSortOrderOnStateChange": func(o teamObj) any { return orString(o.t.SortOnStateChange, "bottom") },
	"archivedAt":                     func(o teamObj) any { return o.t.ArchivedAt },
	"updatedAt":                      func(o teamObj) any { return o.t.UpdatedAt },
	"members": func(o teamObj) any {
		var out []gqlObject
		for _, id := range o.t.Members {
			out = append(out, userObj{o.w, o.w.user(id)})
		}
		return out
	},
}

type stateObj struct {
	w *linearWorkspace
	s *LinearState
}

func (stateObj) typeName() string                { return "WorkflowState" }
func (o stateObj) archived() bool                { return !o.s.ArchivedAt.IsZero() }
func (o stateObj) value(name string) (any, bool) { return stateFields.get(o, name) }

var stateFields = fieldTable[stateObj]{
	"id":          func(o stateObj) any { return o.s.ID },
	"name":        func(o stateObj) any { return o.s.Name },
	"color":       func(o stateObj) any { return orString(o.s.Color, "#bec2c8") },
	"description": func(o stateObj) any { return stringOrNil(o.s.Description) },
	"type":        func(o stateObj) any { return o.s.Type },
	"position":    func(o stateObj) any { return o.s.Position },
	"team":        func(o stateObj) any { return o.w.teamRef(o.s.Team) },
	"archivedAt":  func(o stateObj) any { return o.s.ArchivedAt },
}

type cycleObj struct {
	w *linearWorkspace
	c *LinearCycle
}

func (cycleObj) typeName() string                { return "Cycle" }
func (o cycleObj) archived() bool                { return !o.c.ArchivedAt.IsZero() }
func (o cycleObj) value(name string) (any, bool) { return cycleFields.get(o, name) }

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

var cycleFields = fieldTable[cycleObj]{
	"id":                         func(o cycleObj) any { return o.c.ID },
	"number":                     func(o cycleObj) any { return o.c.Number },
	"name":                       func(o cycleObj) any { return stringOrNil(o.c.Name) },
	"description":                func(o cycleObj) any { return stringOrNil(o.c.Description) },
	"team":                       func(o cycleObj) any { return o.w.teamRef(o.c.Team) },
	"startsAt":                   func(o cycleObj) any { return o.c.StartsAt },
	"endsAt":                     func(o cycleObj) any { return o.c.EndsAt },
	"completedAt":                func(o cycleObj) any { return o.c.CompletedAt },
	"archivedAt":                 func(o cycleObj) any { return o.c.ArchivedAt },
	"isActive":                   func(o cycleObj) any { return o.active() },
	"isFuture":                   func(o cycleObj) any { return o.future() },
	"isPast":                     func(o cycleObj) any { return o.past() },
	"isNext":                     func(o cycleObj) any { return o.neighbor(true) },
	"isPrevious":                 func(o cycleObj) any { return o.neighbor(false) },
	"progress":                   func(o cycleObj) any { return o.progress() },
	"scopeHistory":               func(o cycleObj) any { return orEmpty(o.c.ScopeHistory) },
	"completedScopeHistory":      func(o cycleObj) any { return orEmpty(o.c.CompletedScopeHistory) },
	"inProgressScopeHistory":     func(o cycleObj) any { return orEmpty(o.c.InProgressScopeHistory) },
	"issueCountHistory":          func(o cycleObj) any { return orEmpty(o.c.IssueCountHistory) },
	"completedIssueCountHistory": func(o cycleObj) any { return orEmpty(o.c.CompletedIssueCountHistory) },
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

func (labelObj) typeName() string                { return "IssueLabel" }
func (o labelObj) archived() bool                { return !o.l.ArchivedAt.IsZero() }
func (o labelObj) value(name string) (any, bool) { return labelFields.get(o, name) }

var labelFields = fieldTable[labelObj]{
	"id":          func(o labelObj) any { return o.l.ID },
	"name":        func(o labelObj) any { return o.l.Name },
	"color":       func(o labelObj) any { return orString(o.l.Color, "#bec2c8") },
	"description": func(o labelObj) any { return stringOrNil(o.l.Description) },
	"isGroup":     func(o labelObj) any { return o.l.IsGroup },
	"parent":      func(o labelObj) any { return o.w.labelRef(o.l.Parent) },
	"team":        func(o labelObj) any { return o.w.teamRef(o.l.Team) },
	"archivedAt":  func(o labelObj) any { return o.l.ArchivedAt },
}

type projectObj struct {
	w *linearWorkspace
	p *LinearProject
}

func (projectObj) typeName() string                { return "Project" }
func (o projectObj) archived() bool                { return !o.p.ArchivedAt.IsZero() }
func (o projectObj) value(name string) (any, bool) { return projectFields.get(o, name) }

var projectFields = fieldTable[projectObj]{
	"id":   func(o projectObj) any { return o.p.ID },
	"name": func(o projectObj) any { return o.p.Name },
	"url": func(o projectObj) any {
		return "https://linear.app/" + o.w.URLKey + "/project/" + slug(o.p.Name) + "-" + o.p.ID[len(o.p.ID)-12:]
	},
	"status":      func(o projectObj) any { return projectStatusObj{o.p} },
	"lead":        func(o projectObj) any { return o.w.userRef(o.p.Lead) },
	"startDate":   func(o projectObj) any { return stringOrNil(o.p.StartDate) },
	"targetDate":  func(o projectObj) any { return stringOrNil(o.p.TargetDate) },
	"completedAt": func(o projectObj) any { return o.p.CompletedAt },
	"canceledAt":  func(o projectObj) any { return o.p.CanceledAt },
	"archivedAt":  func(o projectObj) any { return o.p.ArchivedAt },
	"updatedAt":   func(o projectObj) any { return o.p.UpdatedAt },
	"accessibleTeams": func(o projectObj) any {
		var out []gqlObject
		for _, id := range o.p.Teams {
			out = append(out, teamObj{o.w, o.w.team(id)})
		}
		return out
	},
}

type projectStatusObj struct{ p *LinearProject }

func (projectStatusObj) typeName() string                { return "ProjectStatus" }
func (o projectStatusObj) value(name string) (any, bool) { return projectStatusFields.get(o, name) }

var projectStatusFields = fieldTable[projectStatusObj]{
	"id":   func(o projectStatusObj) any { return o.p.StatusID },
	"name": func(o projectStatusObj) any { return o.p.StatusName },
	"type": func(o projectStatusObj) any { return o.p.StatusType },
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

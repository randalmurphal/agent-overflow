package trackerfake

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// linearWorkspace is a seeded workspace: its state, the key that reaches
// it, and its rate limit window.
type linearWorkspace struct {
	LinearWorkspace
	apiKey string
	usage  linearUsage
	// clock is the engine's time of the request being answered, which
	// a cycle's state follows.
	clock time.Time
}

// linearUsage is the workspace's spend in the current hour.
type linearUsage struct {
	start    time.Time
	requests int
	points   int
}

func (w *linearWorkspace) key() string { return w.URLKey }

// linearID is a generated ID in Linear's lowercase UUID form.
func linearID(e *Engine) string { return mediaID(e.nextID()) }

// reserveLinearID keeps generated IDs above a seeded ID of the generated
// form.
func (e *Engine) reserveLinearID(id string) {
	var hi, lo int64
	if _, err := fmt.Sscanf(id, "%08x-0000-4000-8000-%012x", &hi, &lo); err == nil && mediaID(hi<<48|lo) == id {
		e.reserveID(hi<<48 | lo)
	}
}

var linearStateTypes = []string{"triage", "backlog", "unstarted", "started", "completed", "canceled"}

// newLinearWorkspace checks and completes a workspace's fixture. Callers
// hold mu.
func (e *Engine) newLinearWorkspace(in LinearWorkspace) (*linearWorkspace, error) {
	w := &linearWorkspace{LinearWorkspace: in, apiKey: in.APIKey}
	w.APIKey = ""
	fail := func(format string, args ...any) error {
		return fmt.Errorf("trackerfake: Linear workspace %s: "+format, append([]any{in.URLKey}, args...)...)
	}
	if w.URLKey == "" || strings.ContainsAny(w.URLKey, "/ ") {
		return nil, fmt.Errorf("trackerfake: a Linear workspace URL key %q is not a key", w.URLKey)
	}
	if err := checkToken("the API key", w.apiKey); err != nil {
		return nil, fail("%v", err)
	}
	// The first pass reserves every seeded ID, so the second generates
	// none that a seeded one already holds.
	ids := map[string]bool{}
	for _, is := range w.Issues {
		for _, h := range is.History {
			e.reserveLinearID(h.ID)
		}
	}
	for _, assign := range []bool{false, true} {
		id := func(p *string) error {
			switch {
			case !assign:
				e.reserveLinearID(*p)
				return nil
			case *p == "":
				*p = linearID(e)
			case ids[*p]:
				return fmt.Errorf("ID %s repeats", *p)
			}
			ids[*p] = true
			return nil
		}
		for _, err := range []error{
			id(&w.ID),
			eachID(w.Users, func(u *LinearUser) error { return id(&u.ID) }),
			eachID(w.Teams, func(t *LinearTeam) error { return id(&t.ID) }),
			eachID(w.States, func(s *LinearState) error { return id(&s.ID) }),
			eachID(w.Cycles, func(c *LinearCycle) error { return id(&c.ID) }),
			eachID(w.Labels, func(l *LinearLabel) error { return id(&l.ID) }),
			eachID(w.Projects, func(p *LinearProject) error { return id(&p.ID) }),
			eachID(w.Issues, func(is *LinearIssue) error { return id(&is.ID) }),
			eachID(w.Comments, func(c *LinearComment) error { return id(&c.ID) }),
			eachID(w.Attachments, func(a *LinearAttachment) error { return id(&a.ID) }),
			eachID(w.Relations, func(r *LinearRelation) error { return id(&r.ID) }),
		} {
			if err != nil {
				return nil, fail("%v", err)
			}
		}
	}
	if w.user(w.Viewer) == nil {
		return nil, fail("the viewer %q is no user", w.Viewer)
	}
	if err := w.seedTeams(e); err != nil {
		return nil, fail("%v", err)
	}
	if err := w.seedIssues(e); err != nil {
		return nil, fail("%v", err)
	}
	return w, nil
}

func eachID[T any](list []T, f func(*T) error) error {
	for i := range list {
		if err := f(&list[i]); err != nil {
			return err
		}
	}
	return nil
}

func (w *linearWorkspace) seedTeams(e *Engine) error {
	for i := range w.Teams {
		t := &w.Teams[i]
		if t.Key == "" || w.teamByKey(t.Key) != t {
			return fmt.Errorf("team %s has no key or repeats one", t.ID)
		}
		if t.Estimation == "" {
			t.Estimation = "notUsed"
		}
		if t.Timezone == "" {
			t.Timezone = "Etc/UTC"
		}
		t.UpdatedAt = orNow(t.UpdatedAt, e.now())
		for _, m := range t.Members {
			if w.user(m) == nil {
				return fmt.Errorf("team %s member %q is no user", t.Key, m)
			}
		}
		if t.DefaultState == "" {
			for _, s := range w.States {
				if s.Team == t.ID && (s.Type == "backlog" || s.Type == "unstarted") {
					t.DefaultState = s.ID
					break
				}
			}
		}
		if st := w.state(t.DefaultState); st == nil || st.Team != t.ID {
			return fmt.Errorf("team %s has no default state of its own", t.Key)
		}
	}
	for _, s := range w.States {
		if w.team(s.Team) == nil || !slices.Contains(linearStateTypes, s.Type) {
			return fmt.Errorf("state %s needs a team and one of the types %v", s.ID, linearStateTypes)
		}
	}
	for _, c := range w.Cycles {
		if w.team(c.Team) == nil || !c.EndsAt.After(c.StartsAt) {
			return fmt.Errorf("cycle %s needs a team and an end after its start", c.ID)
		}
	}
	for _, l := range w.Labels {
		if (l.Team != "" && w.team(l.Team) == nil) || (l.Parent != "" && w.label(l.Parent) == nil) {
			return fmt.Errorf("label %s names a team or parent the workspace lacks", l.ID)
		}
	}
	for i := range w.Projects {
		p := &w.Projects[i]
		for _, t := range p.Teams {
			if w.team(t) == nil {
				return fmt.Errorf("project %s names team %q, which the workspace lacks", p.ID, t)
			}
		}
		if p.StatusType == "" {
			p.StatusID, p.StatusName, p.StatusType = linearID(e), "Planned", "planned"
		}
		p.UpdatedAt = orNow(p.UpdatedAt, e.now())
	}
	return nil
}

func (w *linearWorkspace) seedIssues(e *Engine) error {
	for i := range w.Issues {
		is := &w.Issues[i]
		t := w.team(is.Team)
		if t == nil {
			return fmt.Errorf("issue %s is in team %q, which the workspace lacks", is.ID, is.Team)
		}
		if is.Number == 0 {
			is.Number = w.nextNumber(t.ID)
		}
		if is.State == "" {
			is.State = t.DefaultState
		}
		if st := w.state(is.State); st == nil || st.Team != t.ID {
			return fmt.Errorf("issue %s has state %q, not one of its team's", w.identifier(is), is.State)
		}
		for _, ref := range []struct{ id, what string }{{is.Assignee, "user"}, {is.Creator, "user"}, {is.Cycle, "cycle"}, {is.Project, "project"}} {
			if ref.id != "" && !w.exists(ref.what, ref.id) {
				return fmt.Errorf("issue %s names %s %q, which the workspace lacks", w.identifier(is), ref.what, ref.id)
			}
		}
		for _, l := range is.Labels {
			if w.label(l) == nil {
				return fmt.Errorf("issue %s has label %q, which the workspace lacks", w.identifier(is), l)
			}
		}
		if is.Priority < 0 || is.Priority > 4 {
			return fmt.Errorf("issue %s has priority %v, not 0 to 4", w.identifier(is), is.Priority)
		}
		is.CreatedAt = orNow(is.CreatedAt, e.now())
		is.UpdatedAt = orNow(is.UpdatedAt, is.CreatedAt)
		for j := range is.History {
			h := &is.History[j]
			if h.ID == "" {
				h.ID = linearID(e)
			}
			h.CreatedAt = orNow(h.CreatedAt, is.CreatedAt)
		}
	}
	for _, is := range w.Issues {
		if is.Parent != "" && w.issue(is.Parent) == nil {
			return fmt.Errorf("issue %s has parent %q, which the workspace lacks", w.identifier(&is), is.Parent)
		}
	}
	for i := range w.Comments {
		c := &w.Comments[i]
		if w.issue(c.Issue) == nil || w.user(c.User) == nil {
			return fmt.Errorf("comment %s needs an issue and a user of the workspace", c.ID)
		}
		c.CreatedAt = orNow(c.CreatedAt, e.now())
		c.UpdatedAt = orNow(c.UpdatedAt, c.CreatedAt)
	}
	for i := range w.Attachments {
		a := &w.Attachments[i]
		if w.issue(a.Issue) == nil || a.URL == "" {
			return fmt.Errorf("attachment %s needs an issue and a URL", a.ID)
		}
		a.CreatedAt = orNow(a.CreatedAt, e.now())
		a.UpdatedAt = orNow(a.UpdatedAt, a.CreatedAt)
	}
	for _, r := range w.Relations {
		if w.issue(r.Issue) == nil || w.issue(r.Related) == nil || !slices.Contains([]string{"blocks", "duplicate", "related", "similar"}, r.Type) {
			return fmt.Errorf("relation %s needs two issues and a type", r.ID)
		}
	}
	return nil
}

// nextNumber is the number a new issue of team gets.
func (w *linearWorkspace) nextNumber(team string) float64 {
	n := 0.0
	for _, is := range w.Issues {
		if is.Team == team {
			n = max(n, is.Number)
		}
	}
	return n + 1
}

func (w *linearWorkspace) exists(what, id string) bool {
	switch what {
	case "user":
		return w.user(id) != nil
	case "cycle":
		return w.cycle(id) != nil
	case "project":
		return w.project(id) != nil
	}
	return false
}

func findBy[T any](list []T, match func(*T) bool) *T {
	for i := range list {
		if match(&list[i]) {
			return &list[i]
		}
	}
	return nil
}

func (w *linearWorkspace) user(id string) *LinearUser {
	return findBy(w.Users, func(u *LinearUser) bool { return u.ID == id })
}

func (w *linearWorkspace) team(id string) *LinearTeam {
	return findBy(w.Teams, func(t *LinearTeam) bool { return t.ID == id })
}

func (w *linearWorkspace) teamByKey(key string) *LinearTeam {
	return findBy(w.Teams, func(t *LinearTeam) bool { return strings.EqualFold(t.Key, key) })
}

func (w *linearWorkspace) state(id string) *LinearState {
	return findBy(w.States, func(s *LinearState) bool { return s.ID == id })
}

func (w *linearWorkspace) cycle(id string) *LinearCycle {
	return findBy(w.Cycles, func(c *LinearCycle) bool { return c.ID == id })
}

func (w *linearWorkspace) label(id string) *LinearLabel {
	return findBy(w.Labels, func(l *LinearLabel) bool { return l.ID == id })
}

func (w *linearWorkspace) project(id string) *LinearProject {
	return findBy(w.Projects, func(p *LinearProject) bool { return p.ID == id })
}

func (w *linearWorkspace) comment(id string) *LinearComment {
	return findBy(w.Comments, func(c *LinearComment) bool { return c.ID == id })
}

func (w *linearWorkspace) relation(id string) *LinearRelation {
	return findBy(w.Relations, func(r *LinearRelation) bool { return r.ID == id })
}

// issue finds an issue by ID or identifier, the key's case ignored.
func (w *linearWorkspace) issue(ref string) *LinearIssue {
	key, num, ok := strings.Cut(ref, "-")
	n, err := strconv.ParseFloat(num, 64)
	byIdentifier := ok && err == nil && !strings.Contains(num, "-")
	return findBy(w.Issues, func(is *LinearIssue) bool {
		if is.ID == ref {
			return true
		}
		t := w.team(is.Team)
		return byIdentifier && is.Number == n && t != nil && strings.EqualFold(t.Key, key)
	})
}

func (w *linearWorkspace) identifier(is *LinearIssue) string {
	return w.team(is.Team).Key + "-" + strconv.FormatFloat(is.Number, 'f', -1, 64)
}

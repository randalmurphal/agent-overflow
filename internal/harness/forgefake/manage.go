package forgefake

import (
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
)

// The fixture of the management actions: who can be asked to review or
// be assigned, the teams and labels, the viewer's capabilities on a
// pull, and what an older GitLab lacks. The handlers are
// manage_github.go and manage_gitlab.go.

// Member is a user the repository can request reviews from and assign:
// a GitHub collaborator (assignableUsers), a GitLab project member
// (projects/P/users). ID is the GitLab user id, generated when omitted.
type Member struct {
	Login string `json:"login"`
	Name  string `json:"name,omitempty"`
	ID    int64  `json:"id,omitempty"`
}

// Team is a GitHub organization team (Repo.Organization), a reviewer as
// "<owner>/<slug>". TeamsRefused answers the teams search as a token
// without read:org does.
type Team struct {
	Slug string `json:"slug"`
	Name string `json:"name,omitempty"`
}

// Label is one of the repository's labels. Color is "#rrggbb"
// ("#ededed" by default); ID is GitHub's node id, generated.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	ID    string `json:"id,omitempty"`
}

// ViewerCan overrides what the viewer may do on a pull, each nil field
// derived from the viewer's role and authorship, each independent of
// the others: update (edit; GitHub draft; GitLab close, reopen and draft:
// GitHub WRITE or the author, GitLab developer or the author), close and
// reopen (GitHub viewerCanClose, viewerCanReopen: WRITE or the author),
// label (GitHub viewerCanLabel: TRIAGE), assign (GitHub viewerCanAssign:
// TRIAGE), triage (GitHub review requests: TRIAGE), admin (GitLab
// reviewers, assignees and labels: developer). Each write refuses as its
// forge does when its flag is false.
type ViewerCan struct {
	Update *bool `json:"update,omitempty"`
	Close  *bool `json:"close,omitempty"`
	Reopen *bool `json:"reopen,omitempty"`
	Label  *bool `json:"label,omitempty"`
	Assign *bool `json:"assign,omitempty"`
	Triage *bool `json:"triage,omitempty"`
	Admin  *bool `json:"admin,omitempty"`
}

// StateEvent is a GitLab resource state event: a close, a reopen
// ("opened", as GitLab records it; "reopened" as some servers do) or a
// merge, which GitLab records as these rather than as system notes. A
// seeded closed or merged merge request without one gets one.
type StateEvent struct {
	ID    int64  `json:"id,omitempty"`
	State string `json:"state"`
	Actor string `json:"actor,omitempty"`
	At    string `json:"at,omitempty"`
}

// The GitLab features an older server lacks (Repo.ServerLacks):
// operationMode is the set mutations' (reviewers, assignees, labels).
const (
	lacksRereview      = "rereview"
	lacksOverride      = "overrideRequestedChanges"
	lacksOperationMode = "operationMode"
)

// githubAssigneesMax is how many assignees GitHub keeps.
const githubAssigneesMax = 10

func (r *Repo) normalizeManage(ids *idSource) error {
	if r.Forge != "github" && (len(r.Teams) > 0 || r.TeamsRefused || r.Organization) {
		return errors.New("teams, teamsRefused and organization are a GitHub organization's")
	}
	if r.Forge != "gitlab" && len(r.ServerLacks) > 0 {
		return errors.New("serverLacks names what an older GitLab lacks")
	}
	for _, lack := range r.ServerLacks {
		if lack != lacksRereview && lack != lacksOverride && lack != lacksOperationMode {
			return fmt.Errorf("serverLacks %q is not %s, %s or %s", lack, lacksRereview, lacksOverride, lacksOperationMode)
		}
	}
	for i := range r.Members {
		m := &r.Members[i]
		if strings.TrimSpace(m.Login) == "" {
			return fmt.Errorf("members[%d]: login is required", i)
		}
		if m.ID == 0 {
			m.ID = ids.next()
		}
	}
	for i, team := range r.Teams {
		if strings.TrimSpace(team.Slug) == "" || strings.Contains(team.Slug, "/") {
			return fmt.Errorf("teams[%d]: slug is required and holds no /", i)
		}
	}
	for i := range r.Labels {
		label := &r.Labels[i]
		if strings.TrimSpace(label.Name) == "" {
			return fmt.Errorf("labels[%d]: a name is required", i)
		}
		label.Color = strings.ToLower(defaultString(label.Color, "#ededed"))
		if label.ID == "" {
			label.ID = "LA_ao" + strconv.FormatInt(ids.next(), 10)
			if r.Forge == "gitlab" {
				label.ID = "gid://gitlab/ProjectLabel/" + strconv.FormatInt(ids.next(), 10)
			}
		}
	}
	for i := range r.Pulls {
		p := &r.Pulls[i]
		for _, name := range p.Labels {
			if r.label(name) == nil {
				return fmt.Errorf("pulls[%d]: label %q is not one of the repository's labels", i, name)
			}
		}
		for _, team := range p.TeamReviewers {
			if r.Forge != "github" || r.team(team) == nil {
				return fmt.Errorf("pulls[%d]: team reviewer %q is not one of the repository's teams (owner/slug)", i, team)
			}
		}
		if r.Forge != "gitlab" && (len(p.StateEvents) > 0 || p.ChangesOverridden) {
			return fmt.Errorf("pulls[%d]: stateEvents and changesOverridden are a GitLab merge request's", i)
		}
		if r.Forge == "gitlab" {
			if err := p.normalizeStateEvents(ids); err != nil {
				return fmt.Errorf("pulls[%d]: %w", i, err)
			}
		}
	}
	return nil
}

func (p *Pull) normalizeStateEvents(ids *idSource) error {
	for i := range p.StateEvents {
		event := &p.StateEvents[i]
		switch event.State {
		case "closed", "opened", "reopened", "merged":
		default:
			return fmt.Errorf("stateEvents[%d]: state %q is not closed, opened, reopened or merged", i, event.State)
		}
		if event.ID == 0 {
			event.ID = ids.next()
		}
		event.Actor = defaultString(event.Actor, p.Author)
		event.At = defaultString(event.At, "2026-01-01T00:00:00Z")
	}
	has := func(state string) bool {
		return slices.ContainsFunc(p.StateEvents, func(e StateEvent) bool { return e.State == state })
	}
	switch {
	case p.State == "closed" && !has("closed"):
		p.StateEvents = append(p.StateEvents, StateEvent{ID: ids.next(), State: "closed", Actor: p.ClosedBy, At: p.ClosedAt})
	case p.State == "merged" && !has("merged"):
		p.StateEvents = append(p.StateEvents, StateEvent{ID: ids.next(), State: "merged", Actor: p.MergedBy, At: p.MergedAt})
	}
	return nil
}

func (r *Repo) label(name string) *Label {
	for i := range r.Labels {
		if strings.EqualFold(r.Labels[i].Name, name) {
			return &r.Labels[i]
		}
	}
	return nil
}

func (r *Repo) labelByID(id string) *Label {
	for i := range r.Labels {
		if r.Labels[i].ID == id {
			return &r.Labels[i]
		}
	}
	return nil
}

// team resolves "<owner>/<slug>" among the repository's teams.
func (r *Repo) team(combined string) *Team {
	owner, slug, ok := strings.Cut(combined, "/")
	repoOwner, _, _ := strings.Cut(r.Project, "/")
	if !ok || !strings.EqualFold(owner, repoOwner) {
		return nil
	}
	for i := range r.Teams {
		if strings.EqualFold(r.Teams[i].Slug, slug) {
			return &r.Teams[i]
		}
	}
	return nil
}

func (r *Repo) member(login string) *Member {
	for i := range r.Members {
		if strings.EqualFold(r.Members[i].Login, login) {
			return &r.Members[i]
		}
	}
	return nil
}

func (r *Repo) lacks(feature string) bool { return slices.Contains(r.ServerLacks, feature) }

// gitlabUserID is a login's GitLab user id: the viewer's 1 (as GET user
// answers it), a member's own, else one derived from the login.
func (e *Engine) gitlabUserID(r *Repo, login string) int64 {
	if strings.EqualFold(login, e.viewer) {
		return 1
	}
	if m := r.member(login); m != nil {
		return m.ID
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(login)))
	return 2_000_000_000 + int64(h.Sum32()%1_000_000)
}

// gitlabLoginOf resolves a user id back to a login among those the
// repository knows: the viewer, its members, and the pulls' people.
func (e *Engine) gitlabLoginOf(r *Repo, id int64) (string, bool) {
	known := []string{e.viewer}
	for _, m := range r.Members {
		known = append(known, m.Login)
	}
	for _, p := range r.Pulls {
		known = append(append(append(known, p.Author), p.Reviewers...), p.Assignees...)
	}
	for _, login := range known {
		if e.gitlabUserID(r, login) == id {
			return login, true
		}
	}
	return "", false
}

// viewerMay is a ViewerCan flag: the override, else derived.
func viewerMay(override *bool, derived bool) bool {
	if override != nil {
		return *override
	}
	return derived
}

func (p *Pull) viewer() ViewerCan {
	if p.Viewer == nil {
		return ViewerCan{}
	}
	return *p.Viewer
}

// mayUpdate is whether the viewer may edit, close, reopen or convert the
// pull: the author, or GitHub WRITE / GitLab developer.
func (e *Engine) mayUpdate(r *Repo, p *Pull) bool {
	role := strings.EqualFold(p.Author, e.viewer)
	if r.Forge == "github" {
		role = role || githubRoles[r.Merge.Permission] >= githubRoles["WRITE"]
	} else {
		role = role || gitlabRoles[r.Merge.Permission] >= gitlabRoles["developer"]
	}
	return viewerMay(p.viewer().Update, role)
}

// githubWriter is GitHub WRITE access or the pull's author.
func (e *Engine) githubWriter(r *Repo, p *Pull) bool {
	return strings.EqualFold(p.Author, e.viewer) || githubRoles[r.Merge.Permission] >= githubRoles["WRITE"]
}

// mayClose is GitHub's viewerCanClose.
func (e *Engine) mayClose(r *Repo, p *Pull) bool {
	return viewerMay(p.viewer().Close, e.githubWriter(r, p))
}

// mayReopen is GitHub's viewerCanReopen.
func (e *Engine) mayReopen(r *Repo, p *Pull) bool {
	return viewerMay(p.viewer().Reopen, e.githubWriter(r, p))
}

// mayAssign is GitHub's viewerCanAssign.
func (e *Engine) mayAssign(r *Repo, p *Pull) bool {
	return viewerMay(p.viewer().Assign, githubRoles[r.Merge.Permission] >= githubRoles["TRIAGE"])
}

// mayTriage is GitHub's triage access to request reviews.
func (e *Engine) mayTriage(r *Repo, p *Pull) bool {
	return viewerMay(p.viewer().Triage, githubRoles[r.Merge.Permission] >= githubRoles["TRIAGE"])
}

// mayLabel is GitHub's viewerCanLabel.
func (e *Engine) mayLabel(r *Repo, p *Pull) bool {
	return viewerMay(p.viewer().Label, githubRoles[r.Merge.Permission] >= githubRoles["TRIAGE"])
}

// mayAdmin is GitLab's adminMergeRequest.
func (e *Engine) mayAdmin(r *Repo, p *Pull) bool {
	return viewerMay(p.viewer().Admin, gitlabRoles[r.Merge.Permission] >= gitlabRoles["developer"])
}

// matches is a search's substring match on a login or a name.
func matches(query string, values ...string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func containsFold(list []string, value string) bool {
	return slices.ContainsFunc(list, func(other string) bool { return strings.EqualFold(other, value) })
}

func removeFold(list []string, value string) []string {
	return slices.DeleteFunc(list, func(other string) bool { return strings.EqualFold(other, value) })
}

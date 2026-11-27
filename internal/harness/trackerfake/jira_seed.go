package trackerfake

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// jiraSite is a seeded site: its state and the credentials that reach it,
// which the state and every snapshot leave out.
type jiraSite struct {
	JiraSite
	token      string
	tempoToken string
	// members are the other people's tokens by account ID.
	members map[string]string
	// issued are the context tokens the site signed, by token
	// (jira_poker.go).
	issued map[string]issuedToken
	// nextKey is each project's next issue number.
	nextKey map[string]int
}

// The site's defaults, seeded when a fixture leaves them out: Jira's own
// status categories, priorities, a resolution and link types, and the
// Jira Software sprint and rank fields.
var (
	defaultStatuses = []JiraStatus{
		{ID: "10000", Name: "To Do", Category: "new"},
		{ID: "10001", Name: "In Progress", Category: "indeterminate"},
		{ID: "10002", Name: "Done", Category: "done"},
	}
	defaultPriorities = []JiraNamed{
		{ID: "1", Name: "Highest"}, {ID: "2", Name: "High"}, {ID: "3", Name: "Medium"}, {ID: "4", Name: "Low"}, {ID: "5", Name: "Lowest"},
	}
	defaultResolutions = []JiraNamed{{ID: "10000", Name: "Done"}}
	defaultLinkTypes   = []JiraLinkType{
		{ID: "10000", Name: "Blocks", Inward: "is blocked by", Outward: "blocks"},
		{ID: "10003", Name: "Relates", Inward: "relates to", Outward: "relates to"},
	}
	defaultSprintField = JiraField{ID: "customfield_10020", Name: "Sprint", Schema: JiraSchema{
		Type: "array", Items: "json", Custom: "com.pyxis.greenhopper.jira:gh-sprint", CustomID: 10020}}
	defaultRankField = JiraField{ID: "customfield_10019", Name: "Rank", Schema: JiraSchema{
		Type: "any", Custom: "com.pyxis.greenhopper.jira:gh-lexo-rank", CustomID: 10019}}
)

// defaultIssueTypes are a project's types when it names none.
var defaultIssueTypes = []JiraIssueType{
	{ID: "10000", Name: "Epic", HierarchyLevel: 1},
	{ID: "10001", Name: "Story"},
	{ID: "10002", Name: "Task"},
	{ID: "10003", Name: "Bug"},
	{ID: "10004", Name: "Subtask", Subtask: true, HierarchyLevel: -1},
}

// newJiraSite checks and completes a site's fixture. Callers hold mu.
func (e *Engine) newJiraSite(in JiraSite) (*jiraSite, error) {
	s := &jiraSite{JiraSite: in, token: in.Token, nextKey: map[string]int{}, issued: map[string]issuedToken{}}
	s.Token = ""
	fail := func(format string, args ...any) error {
		return fmt.Errorf("trackerfake: Jira site %s: "+format, append([]any{in.Site}, args...)...)
	}
	switch {
	case s.Site == "" || strings.ContainsAny(s.Site, "/ "):
		return nil, fmt.Errorf("trackerfake: a Jira site %q is not a host", s.Site)
	case checkToken("the token", s.token) != nil:
		return nil, fail("%v", checkToken("the token", s.token))
	}
	if err := s.seedPeople(); err != nil {
		return nil, fail("%v", err)
	}
	if err := s.seedCatalog(); err != nil {
		return nil, fail("%v", err)
	}
	if err := s.seedProjects(); err != nil {
		return nil, fail("%v", err)
	}
	if err := s.seedBoards(e); err != nil {
		return nil, fail("%v", err)
	}
	if err := s.seedIssues(e); err != nil {
		return nil, fail("%v", err)
	}
	if err := s.seedPoker(); err != nil {
		return nil, fail("%v", err)
	}
	if s.Tempo != nil {
		if err := s.seedTempo(e); err != nil {
			return nil, fail("tempo: %v", err)
		}
	}
	return s, nil
}

func (s *jiraSite) seedPeople() error {
	seen := map[string]bool{}
	for i := range s.Users {
		u := &s.Users[i]
		if u.AccountID == "" || seen[u.AccountID] {
			return fmt.Errorf("user %d has no account ID or repeats %q", i, u.AccountID)
		}
		seen[u.AccountID] = true
		if u.AccountType == "" {
			u.AccountType = "atlassian"
		}
		if u.TimeZone == "" {
			u.TimeZone = "UTC"
		}
	}
	me := s.user(s.Account)
	if me == nil || me.Email == "" {
		return fmt.Errorf("the account %q is no user with an email", s.Account)
	}
	return nil
}

func (s *jiraSite) seedCatalog() error {
	if len(s.Statuses) == 0 {
		s.Statuses = slices.Clone(defaultStatuses)
	}
	for _, st := range s.Statuses {
		if _, ok := statusCategories[st.Category]; !ok || st.ID == "" {
			return fmt.Errorf("status %q has category %q, not new, indeterminate or done", st.ID, st.Category)
		}
	}
	if len(s.Priorities) == 0 {
		s.Priorities = slices.Clone(defaultPriorities)
	}
	if len(s.Resolutions) == 0 {
		s.Resolutions = slices.Clone(defaultResolutions)
	}
	if len(s.LinkTypes) == 0 {
		s.LinkTypes = slices.Clone(defaultLinkTypes)
	}
	if !slices.ContainsFunc(s.Fields, func(f JiraField) bool { return f.Schema.Custom == sprintFieldType }) {
		s.Fields = append(s.Fields, defaultSprintField)
	}
	if !slices.ContainsFunc(s.Fields, func(f JiraField) bool { return f.Schema.Custom == rankFieldType }) {
		s.Fields = append(s.Fields, defaultRankField)
	}
	seen := map[string]bool{}
	for _, f := range s.Fields {
		switch {
		case !strings.HasPrefix(f.ID, "customfield_") || seen[f.ID]:
			return fmt.Errorf("custom field %q is not customfield_<n> or repeats", f.ID)
		case systemField(f.ID) != nil:
			return fmt.Errorf("custom field %q is a system field", f.ID)
		case customKind(f.Schema) == kindUnsupported:
			return fmt.Errorf("custom field %s has a schema the fake does not model (%s %s %s)", f.ID, f.Schema.Type, f.Schema.Items, f.Schema.Custom)
		}
		seen[f.ID] = true
	}
	return nil
}

func (s *jiraSite) seedProjects() error {
	seen := map[string]bool{}
	for i := range s.Projects {
		p := &s.Projects[i]
		if p.ID == "" || p.Key == "" || seen[p.ID] || seen[p.Key] {
			return fmt.Errorf("project %d has no ID or key, or repeats one", i)
		}
		seen[p.ID], seen[p.Key] = true, true
		if p.TypeKey == "" {
			p.TypeKey = "software"
		}
		if len(p.Statuses) == 0 {
			for _, st := range s.Statuses {
				p.Statuses = append(p.Statuses, st.ID)
			}
		}
		for _, id := range p.Statuses {
			if s.status(id) == nil {
				return fmt.Errorf("project %s names status %q, which the site lacks", p.Key, id)
			}
		}
		if len(p.IssueTypes) == 0 {
			p.IssueTypes = slices.Clone(defaultIssueTypes)
		}
		for j := range p.IssueTypes {
			t := &p.IssueTypes[j]
			if len(t.Screen) == 0 {
				t.Screen = s.defaultScreen()
			}
			if err := s.checkScreen(t.Screen); err != nil {
				return fmt.Errorf("project %s type %s: %w", p.Key, t.Name, err)
			}
		}
		if len(p.Transitions) == 0 {
			for j, id := range p.Statuses {
				p.Transitions = append(p.Transitions, JiraTransition{ID: strconv.Itoa(11 + 10*j), Name: s.status(id).Name, To: id})
			}
		}
		for _, t := range p.Transitions {
			if !slices.Contains(p.Statuses, t.To) {
				return fmt.Errorf("project %s transition %s goes to %q, not one of its statuses", p.Key, t.ID, t.To)
			}
			if err := s.checkScreen(t.Screen); err != nil {
				return fmt.Errorf("project %s transition %s: %w", p.Key, t.ID, err)
			}
		}
		for _, id := range p.Assignable {
			if s.user(id) == nil {
				return fmt.Errorf("project %s: assignable %q is no user", p.Key, id)
			}
		}
	}
	return nil
}

// defaultScreen is a type's screen when it names none: the summary,
// required, the common system fields and every custom field but rank.
func (s *jiraSite) defaultScreen() []JiraScreenField {
	out := []JiraScreenField{{Field: "summary", Required: true}}
	for _, id := range []string{"description", "assignee", "reporter", "priority", "labels", "duedate", "parent", "components"} {
		out = append(out, JiraScreenField{Field: id})
	}
	for _, f := range s.Fields {
		if f.Schema.Custom != rankFieldType {
			out = append(out, JiraScreenField{Field: f.ID})
		}
	}
	return out
}

func (s *jiraSite) checkScreen(screen []JiraScreenField) error {
	seen := map[string]bool{}
	for _, sf := range screen {
		if seen[sf.Field] {
			return fmt.Errorf("screen names %s twice", sf.Field)
		}
		seen[sf.Field] = true
		if s.field(sf.Field) == nil {
			return fmt.Errorf("screen field %q is not a field of the site", sf.Field)
		}
	}
	return nil
}

func (s *jiraSite) seedBoards(e *Engine) error {
	seen := map[int64]bool{}
	for i := range s.Boards {
		b := &s.Boards[i]
		switch {
		case b.ID <= 0 || seen[b.ID]:
			return fmt.Errorf("board %d has no ID or repeats one", i)
		case b.Type != "scrum" && b.Type != "kanban" && b.Type != "simple":
			return fmt.Errorf("board %d has type %q", b.ID, b.Type)
		case s.project(b.Project) == nil:
			return fmt.Errorf("board %d is in project %q, which the site lacks", b.ID, b.Project)
		case b.Estimation != "" && s.field(b.Estimation) == nil:
			return fmt.Errorf("board %d estimates in %q, which is no field", b.ID, b.Estimation)
		}
		for _, ref := range b.Also {
			if p := s.project(ref); p == nil || p == s.project(b.Project) {
				return fmt.Errorf("board %d's filter also names %q, which is no other project of the site", b.ID, ref)
			}
		}
		seen[b.ID] = true
		e.reserveID(b.ID)
		if b.FilterID == "" {
			b.FilterID = strconv.FormatInt(10000+b.ID, 10)
		}
		// A saved filter selects its projects' issues (filter = <n> in
		// JQL), so the boards sharing one share the location's project.
		if other := s.boardOfFilter(b.FilterID); other != b && s.project(other.Project) != s.project(b.Project) {
			return fmt.Errorf("board %d shares filter %s with board %d of another project", b.ID, b.FilterID, other.ID)
		}
		if b.SubQuery != "" {
			if b.Type != "kanban" {
				return fmt.Errorf("board %d has a sub-query and is not a kanban board", b.ID)
			}
			if _, err := parseJQL(b.SubQuery, ""); err != nil {
				return fmt.Errorf("board %d sub-query: %w", b.ID, err)
			}
		}
		for _, col := range b.Columns {
			for _, id := range col.Statuses {
				if s.status(id) == nil {
					return fmt.Errorf("board %d column %q maps status %q, which the site lacks", b.ID, col.Name, id)
				}
			}
		}
	}
	seen = map[int64]bool{}
	for i := range s.Sprints {
		sp := &s.Sprints[i]
		switch {
		case sp.ID <= 0 || seen[sp.ID]:
			return fmt.Errorf("sprint %d has no ID or repeats one", i)
		case s.board(sp.Board) == nil:
			return fmt.Errorf("sprint %d is on board %d, which the site lacks", sp.ID, sp.Board)
		case sp.State != "future" && sp.State != "active" && sp.State != "closed":
			return fmt.Errorf("sprint %d has state %q", sp.ID, sp.State)
		}
		for _, b := range sp.Shown {
			if s.board(b) == nil || b == sp.Board {
				return fmt.Errorf("sprint %d is shown on board %d, which the site lacks or it was made on", sp.ID, b)
			}
		}
		seen[sp.ID] = true
		e.reserveID(sp.ID)
	}
	return nil
}

func (s *jiraSite) seedIssues(e *Engine) error {
	seen := map[string]bool{}
	for i := range s.Issues {
		is := &s.Issues[i]
		p := s.project(is.Project)
		if p == nil {
			return fmt.Errorf("issue %d is in project %q, which the site lacks", i, is.Project)
		}
		if is.ID == "" {
			is.ID = strconv.FormatInt(e.nextID(), 10)
		} else if n, err := strconv.ParseInt(is.ID, 10, 64); err != nil || n <= 0 {
			return fmt.Errorf("issue ID %q is not numeric", is.ID)
		} else {
			e.reserveID(n)
		}
		if err := s.keyIssue(is, p); err != nil {
			return err
		}
		if seen[is.ID] || seen[is.Key] {
			return fmt.Errorf("issue %s %s repeats an ID or key", is.ID, is.Key)
		}
		seen[is.ID], seen[is.Key] = true, true
		for _, k := range is.FormerKeys {
			// A key is not given again: a project's next number passes its
			// former keys too.
			prefix, num, _ := strings.Cut(k, "-")
			n, err := strconv.Atoi(num)
			if seen[k] || prefix == "" || err != nil || n <= 0 {
				return fmt.Errorf("issue %s: former key %q repeats a key or is not <project>-<n>", is.Key, k)
			}
			seen[k] = true
			s.nextKey[prefix] = max(s.nextKey[prefix], n+1)
		}
		if err := s.completeIssue(e, is, p); err != nil {
			return fmt.Errorf("issue %s: %w", is.Key, err)
		}
	}
	for _, is := range s.Issues {
		if is.Parent != "" && s.issue(is.Parent) == nil {
			return fmt.Errorf("issue %s has parent %q, which the site lacks", is.Key, is.Parent)
		}
	}
	return s.seedLinks(e)
}

// keyIssue gives is its key, or checks the one it has, and moves the
// project's next number past it.
func (s *jiraSite) keyIssue(is *JiraIssue, p *JiraProject) error {
	if is.Key == "" {
		s.nextKey[p.Key] = max(s.nextKey[p.Key], 1)
		is.Key = p.Key + "-" + strconv.Itoa(s.nextKey[p.Key])
	}
	prefix, num, ok := strings.Cut(is.Key, "-")
	n, err := strconv.Atoi(num)
	if !ok || prefix != p.Key || err != nil || n <= 0 {
		return fmt.Errorf("issue key %q is not %s-<n>", is.Key, p.Key)
	}
	s.nextKey[p.Key] = max(s.nextKey[p.Key], n+1)
	return nil
}

// completeIssue fills an issue's defaults and checks what it names.
func (s *jiraSite) completeIssue(e *Engine, is *JiraIssue, p *JiraProject) error {
	if is.Type == "" {
		is.Type = p.IssueTypes[0].ID
	}
	if p.issueType(is.Type) == nil {
		return fmt.Errorf("type %q is not one of project %s's", is.Type, p.Key)
	}
	if is.Status == "" {
		is.Status = p.Statuses[0]
	}
	if !slices.Contains(p.Statuses, is.Status) {
		return fmt.Errorf("status %q is not one of project %s's", is.Status, p.Key)
	}
	for _, id := range []string{is.Assignee, is.Reporter} {
		if id != "" && s.user(id) == nil {
			return fmt.Errorf("names user %q, which the site lacks", id)
		}
	}
	if is.Priority != "" && s.priority(is.Priority) == nil {
		return fmt.Errorf("priority %q is no priority", is.Priority)
	}
	for _, id := range is.Sprints {
		if s.sprint(id) == nil {
			return fmt.Errorf("sprint %d is no sprint", id)
		}
	}
	now := e.now()
	if is.Created.IsZero() {
		is.Created = now
	}
	if is.Updated.IsZero() {
		is.Updated = is.Created
	}
	if is.Rank == "" {
		is.Rank = s.lastRank()
	}
	for id, raw := range is.Values {
		if s.field(id) == nil || !json.Valid(raw) {
			return fmt.Errorf("value of %q is not JSON of a field of the site", id)
		}
	}
	if err := s.seedRecords(e, is); err != nil {
		return err
	}
	return nil
}

// seedRecords numbers an issue's comments, worklogs, attachments,
// remote links and changelog entries.
func (s *jiraSite) seedRecords(e *Engine, is *JiraIssue) error {
	ids := func(n int, get func(i int) *string) error {
		for i := range n {
			id := get(i)
			if *id == "" {
				*id = strconv.FormatInt(e.nextID(), 10)
				continue
			}
			v, err := strconv.ParseInt(*id, 10, 64)
			if err != nil || v <= 0 {
				return fmt.Errorf("record ID %q is not numeric", *id)
			}
			e.reserveID(v)
		}
		return nil
	}
	for _, err := range []error{
		ids(len(is.Comments), func(i int) *string { return &is.Comments[i].ID }),
		ids(len(is.Worklogs), func(i int) *string { return &is.Worklogs[i].ID }),
		ids(len(is.Attachments), func(i int) *string { return &is.Attachments[i].ID }),
		ids(len(is.RemoteLinks), func(i int) *string { return &is.RemoteLinks[i].ID }),
		ids(len(is.Changelog), func(i int) *string { return &is.Changelog[i].ID }),
	} {
		if err != nil {
			return err
		}
	}
	for i := range is.Comments {
		c := &is.Comments[i]
		if s.user(c.Author) == nil || !validDoc(c.Body) {
			return fmt.Errorf("comment %s needs an author of the site and an ADF body", c.ID)
		}
		c.Created, c.Updated = orNow(c.Created, is.Created), orNow(c.Updated, orNow(c.Created, is.Created))
	}
	for i := range is.Worklogs {
		w := &is.Worklogs[i]
		if s.user(w.Author) == nil || w.TimeSpent <= 0 || w.Started.IsZero() {
			return fmt.Errorf("worklog %s needs an author of the site, a start and time spent", w.ID)
		}
		w.Created, w.Updated = orNow(w.Created, is.Created), orNow(w.Updated, orNow(w.Created, is.Created))
	}
	for i := range is.Attachments {
		a := &is.Attachments[i]
		if a.Filename == "" {
			return fmt.Errorf("attachment %s has no file name", a.ID)
		}
		if a.MediaID == "" {
			a.MediaID = mediaID(e.nextID())
		}
		a.Created = orNow(a.Created, is.Created)
	}
	return nil
}

func (s *jiraSite) seedLinks(e *Engine) error {
	for i := range s.Links {
		l := &s.Links[i]
		if l.ID == "" {
			l.ID = strconv.FormatInt(e.nextID(), 10)
		}
		if s.linkType(l.Type) == nil || s.issue(l.Outward) == nil || s.issue(l.Inward) == nil {
			return fmt.Errorf("link %s names a type or issue the site lacks", l.ID)
		}
	}
	return nil
}

// orNow is t, or fallback when t is zero.
func orNow(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

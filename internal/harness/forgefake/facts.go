package forgefake

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The facts reads (internal/git forge_facts_*.go): one document per slot
// count, ForgeFactsN, each slot's parts behind @include variables. GitHub
// slot N reads rN, a repository's issueOrPullRequest, or uN, a repository
// owner; GitLab slot N reads kN, a project's issue, merge request, label,
// milestone or snippet, vN, a user, and wN, a group and its epic. The
// handlers answer from the repositories' issues, pulls, labels, milestones
// and snippets and the fixture's accounts on the request's host, as the
// forges answer a missing node: GitHub a null beside a NOT_FOUND error at
// its alias, GitLab a null or no node.

// The slot counts the app's facts documents have.
var (
	githubFactSlots = []int{8, 32, 128}
	gitlabFactSlots = []int{4, 16, 32}
)

// Account is a user, organization (GitHub) or group (GitLab) a forge host
// knows, which the facts reads answer for a user: key.
type Account struct {
	Forge string `json:"forge"`
	// Host defaults to github.com or gitlab.com.
	Host string `json:"host,omitempty"`
	// Login is the GitHub login, or the GitLab username or group path.
	Login string `json:"login"`
	// Name is the display name; empty answers null.
	Name string `json:"name,omitempty"`
	// Kind is "user" (default), "org" (GitHub) or "group" (GitLab).
	Kind string `json:"kind,omitempty"`
	// Epics are a GitLab group's epics.
	Epics []Issue `json:"epics,omitempty"`
}

// Issue is a repository's issue, or a GitLab group's epic. GitHub
// numbers issues and pulls in one sequence, so an issue may not share a
// pull's number there.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	// State is "open" (default) or "closed".
	State string `json:"state,omitempty"`
}

// Milestone is a GitLab project's milestone, or, with Group, one of an
// ancestor group's, which the project's milestones read includes.
type Milestone struct {
	Title string `json:"title"`
	// IID numbers it in its project or group; generated when omitted.
	IID   int    `json:"iid,omitempty"`
	Group string `json:"group,omitempty"`
}

// Snippet is a GitLab project snippet.
type Snippet struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

func (a *Account) normalize() error {
	switch a.Forge {
	case "github":
		a.Host = defaultString(a.Host, "github.com")
		a.Kind = defaultString(a.Kind, "user")
		if a.Kind != "user" && a.Kind != "org" {
			return fmt.Errorf("a GitHub account's kind %q is not user or org", a.Kind)
		}
	case "gitlab":
		a.Host = defaultString(a.Host, "gitlab.com")
		a.Kind = defaultString(a.Kind, "user")
		if a.Kind != "user" && a.Kind != "group" {
			return fmt.Errorf("a GitLab account's kind %q is not user or group", a.Kind)
		}
	default:
		return fmt.Errorf("forge %q is not github or gitlab", a.Forge)
	}
	if strings.TrimSpace(a.Login) == "" {
		return errors.New("an account needs a login")
	}
	if len(a.Epics) > 0 && a.Kind != "group" {
		return errors.New("epics are a GitLab group's")
	}
	return normalizeIssues(a.Epics, nil)
}

func (a *Account) key() string {
	return a.Forge + "\x00" + strings.ToLower(a.Host) + "\x00" + strings.ToLower(a.Login)
}

func normalizeIssues(issues []Issue, taken map[int]bool) error {
	seen := map[int]bool{}
	for i := range issues {
		issue := &issues[i]
		issue.State = defaultString(issue.State, "open")
		switch {
		case issue.Number <= 0:
			return fmt.Errorf("issue %d: a positive number is required", i)
		case seen[issue.Number] || taken[issue.Number]:
			return fmt.Errorf("issue #%d is declared twice", issue.Number)
		case issue.State != "open" && issue.State != "closed":
			return fmt.Errorf("issue #%d: state %q is not open or closed", issue.Number, issue.State)
		}
		seen[issue.Number] = true
	}
	return nil
}

// normalizeFacts checks the repository's issues, milestones and snippets.
func (r *Repo) normalizeFacts() error {
	var pulls map[int]bool
	if r.Forge == "github" {
		pulls = map[int]bool{}
		for _, p := range r.Pulls {
			pulls[p.Number] = true
		}
	}
	if err := normalizeIssues(r.Issues, pulls); err != nil {
		return err
	}
	if r.Forge != "gitlab" && (len(r.Milestones) > 0 || len(r.Snippets) > 0) {
		return errors.New("milestones and snippets are a GitLab project's")
	}
	for i := range r.Milestones {
		m := &r.Milestones[i]
		if strings.TrimSpace(m.Title) == "" {
			return fmt.Errorf("milestones[%d]: a title is required", i)
		}
		if m.IID == 0 {
			m.IID = i + 1
		}
	}
	for i, s := range r.Snippets {
		if s.ID <= 0 {
			return fmt.Errorf("snippets[%d]: a positive id is required", i)
		}
	}
	return nil
}

// factOps are the facts operations of a forge's slot counts.
func factOps(slots []int, vars []string, run func(slots int) func(e *Engine, c *call, v graphQLVars) response) []graphQLOp {
	ops := make([]graphQLOp, 0, len(slots))
	for _, n := range slots {
		required := make([]string, 0, n*len(vars))
		for i := range n {
			for _, v := range vars {
				required = append(required, v+strconv.Itoa(i))
			}
		}
		ops = append(ops, graphQLOp{kind: "query", name: "ForgeFacts" + strconv.Itoa(n), required: required, run: run(n)})
	}
	return ops
}

func init() {
	githubGraphQL = append(githubGraphQL, factOps(githubFactSlots, []string{"o", "n", "x", "r", "u"}, ghFacts)...)
	gitlabGraphQL = append(gitlabGraphQL, factOps(gitlabFactSlots, []string{"p", "n", "t", "s", "q", "i", "m", "l", "ms", "sn", "u", "g", "e"}, glabFacts)...)
}

// account is the fixture's account of forge on host by login, nil when
// none. Callers hold mu.
func (e *Engine) account(forge, host, login string) *Account {
	return e.accounts[forge+"\x00"+strings.ToLower(host)+"\x00"+strings.ToLower(login)]
}

// ghFacts answers GitHub's ForgeFactsN.
func ghFacts(slots int) func(e *Engine, c *call, v graphQLVars) response {
	return func(e *Engine, c *call, v graphQLVars) response {
		data := map[string]any{}
		var errs []map[string]any
		notFound := func(path []string, message string) {
			errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": path, "message": message})
		}
		for i := range slots {
			n := strconv.Itoa(i)
			owner, err1 := v.str("o" + n)
			name, err2 := v.str("n" + n)
			number, err3 := v.integer("x" + n)
			ref, err4 := v.boolean("r" + n)
			user, err5 := v.boolean("u" + n)
			if err := firstErr(err1, err2, err3, err4, err5); err != nil {
				return unhandled("GitHub facts read: %v", err)
			}
			if ref {
				r := e.repoOn("github", c.http.host, owner+"/"+name)
				switch {
				case r == nil:
					data["r"+n] = nil
					notFound([]string{"r" + n}, fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name))
				default:
					item := ghFactItem(r, number)
					data["r"+n] = map[string]any{"issueOrPullRequest": item}
					if item == nil {
						notFound([]string{"r" + n, "issueOrPullRequest"}, fmt.Sprintf("Could not resolve to an issue or pull request with the number of %d.", number))
					}
				}
			}
			if user {
				a := e.account("github", c.http.host, owner)
				if a == nil {
					data["u"+n] = nil
					continue
				}
				typename := "User"
				if a.Kind == "org" {
					typename = "Organization"
				}
				data["u"+n] = map[string]any{"__typename": typename, "login": a.Login, "url": "https://" + a.Host + "/" + a.Login, "name": nullable(a.Name)}
			}
		}
		if len(errs) == 0 {
			return graphQLData(data)
		}
		return jsonResponse(map[string]any{"data": data, "errors": errs})
	}
}

// ghFactItem is issueOrPullRequest(number:) of r, nil when it has none.
func ghFactItem(r *Repo, number int) map[string]any {
	for i := range r.Pulls {
		if p := &r.Pulls[i]; p.Number == number {
			return map[string]any{"__typename": "PullRequest", "title": p.Title, "prState": strings.ToUpper(defaultString(p.State, "open")), "url": githubPullURL(r, p)}
		}
	}
	for _, issue := range r.Issues {
		if issue.Number == number {
			return map[string]any{"__typename": "Issue", "title": issue.Title, "state": strings.ToUpper(issue.State), "url": fmt.Sprintf("https://%s/%s/issues/%d", r.Host, r.Project, number)}
		}
	}
	return nil
}

// glabFacts answers GitLab's ForgeFactsN.
func glabFacts(slots int) func(e *Engine, c *call, v graphQLVars) response {
	return func(e *Engine, c *call, v graphQLVars) response {
		data := map[string]any{}
		for i := range slots {
			n := strconv.Itoa(i)
			path, err1 := v.str("p" + n)
			number, err2 := v.str("n" + n)
			title, err3 := v.str("t" + n)
			snippet, err4 := v.str("s" + n)
			if err := firstErr(err1, err2, err3, err4); err != nil {
				return unhandled("GitLab facts read: %v", err)
			}
			if !strings.HasPrefix(snippet, "gid://gitlab/ProjectSnippet/") {
				return unhandled("GitLab facts read: %q is not a SnippetID", snippet)
			}
			flags := map[string]bool{}
			for _, flag := range []string{"q", "i", "m", "l", "ms", "sn", "u", "g", "e"} {
				on, err := v.boolean(flag + n)
				if err != nil {
					return unhandled("GitLab facts read: %v", err)
				}
				flags[flag] = on
			}
			if flags["q"] {
				data["k"+n] = e.glabFactProject(c.http.host, path, number, title, snippet, flags)
			}
			if flags["u"] {
				if a := e.account("gitlab", c.http.host, title); a != nil && a.Kind == "user" {
					data["v"+n] = map[string]any{"name": a.Name, "webUrl": "https://" + a.Host + "/" + a.Login}
				} else {
					data["v"+n] = nil
				}
			}
			if flags["g"] {
				a := e.account("gitlab", c.http.host, path)
				if a == nil || a.Kind != "group" {
					data["w"+n] = nil
					continue
				}
				group := map[string]any{"name": nullable(a.Name), "webUrl": "https://" + a.Host + "/groups/" + a.Login}
				if flags["e"] {
					group["epic"] = nil
					for _, epic := range a.Epics {
						if strconv.Itoa(epic.Number) == number {
							group["epic"] = map[string]any{"title": epic.Title, "state": strings.ToUpper(epic.State), "webUrl": fmt.Sprintf("https://%s/groups/%s/-/epics/%d", a.Host, a.Login, epic.Number)}
						}
					}
				}
				data["w"+n] = group
			}
		}
		return graphQLData(data)
	}
}

// glabFactProject is a facts slot's project part, nil for a project not
// on host.
func (e *Engine) glabFactProject(host, path, number, title, snippet string, flags map[string]bool) any {
	r := e.repoOn("gitlab", host, path)
	if r == nil {
		return nil
	}
	project := map[string]any{}
	if flags["i"] {
		project["issue"] = nil
		for _, issue := range r.Issues {
			if strconv.Itoa(issue.Number) == number {
				project["issue"] = map[string]any{"title": issue.Title, "state": glabFactState(issue.State), "webUrl": fmt.Sprintf("https://%s/%s/-/issues/%d", r.Host, r.Project, issue.Number)}
			}
		}
	}
	if flags["m"] {
		project["mergeRequest"] = nil
		for i := range r.Pulls {
			if p := &r.Pulls[i]; strconv.Itoa(p.Number) == number {
				project["mergeRequest"] = map[string]any{"title": p.Title, "state": glabFactState(defaultString(p.State, "open")), "webUrl": gitlabMRURL(r, p)}
			}
		}
	}
	if flags["l"] {
		nodes := []map[string]any{}
		for _, label := range r.Labels {
			if label.Name == title {
				nodes = append(nodes, map[string]any{"title": label.Name, "color": label.Color, "textColor": label.TextColor})
				break
			}
		}
		project["labels"] = map[string]any{"nodes": nodes}
	}
	if flags["ms"] {
		nodes := []map[string]any{}
		for _, m := range r.Milestones {
			if m.Title != title {
				continue
			}
			webPath := fmt.Sprintf("/%s/-/milestones/%d", r.Project, m.IID)
			if m.Group != "" {
				webPath = fmt.Sprintf("/groups/%s/-/milestones/%d", m.Group, m.IID)
			}
			nodes = append(nodes, map[string]any{"title": m.Title, "webPath": webPath})
			break
		}
		project["milestones"] = map[string]any{"nodes": nodes}
	}
	if flags["sn"] {
		nodes := []map[string]any{}
		for _, s := range r.Snippets {
			if "gid://gitlab/ProjectSnippet/"+strconv.FormatInt(s.ID, 10) == snippet {
				nodes = append(nodes, map[string]any{"title": s.Title, "webUrl": fmt.Sprintf("https://%s/%s/-/snippets/%d", r.Host, r.Project, s.ID)})
			}
		}
		project["snippets"] = map[string]any{"nodes": nodes}
	}
	return project
}

// glabFactState is a fixture state as GitLab spells an issue's or a merge
// request's (a work item's, an epic's, is upper case).
func glabFactState(state string) string {
	if state == "open" {
		return "opened"
	}
	return state
}

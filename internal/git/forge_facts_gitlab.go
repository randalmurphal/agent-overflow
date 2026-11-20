package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitLab facts: every forge kind. Labels, milestones, users and groups
// have no field that reads several by name, so, as on GitHub, each key is
// an aliased slot of a document built at init per slot count
// (gitlabFactsQueries), every part behind @include. Slot N is kN, its
// project (an issue, merge request, label of the project or an ancestor
// group, milestone of the project or an ancestor, or project snippet), vN,
// a user, and wN, a group with an epic (its work item of type epic). A
// user: key asks vN and wN together: a user's answer wins, else a
// group's, so a group mention costs no second request. GitLab answers a
// missing or hidden project, group, user, issue or merge request with
// null, and a missing label, milestone or snippet with no node: absent.
//
// GitLab refuses a document over its complexity limit (250 for a signed
// in user). Skipped fields are not counted, and a used slot costs at most
// about seven, so the largest document is 32 slots; a refusal still
// reads again in halves (gitlabFactsTooCostly).

// gitlabFactsQuery is the facts document with slots slots.
func gitlabFactsQuery(slots int) forgeapi.Document {
	var vars, body strings.Builder
	for i := range slots {
		if i > 0 {
			vars.WriteString(", ")
		}
		fmt.Fprintf(&vars, "$p%[1]d: ID!, $n%[1]d: String!, $t%[1]d: String!, $s%[1]d: SnippetID!, $q%[1]d: Boolean!, $i%[1]d: Boolean!, $m%[1]d: Boolean!, $l%[1]d: Boolean!, $ms%[1]d: Boolean!, $sn%[1]d: Boolean!, $u%[1]d: Boolean!, $g%[1]d: Boolean!, $e%[1]d: Boolean!", i)
		fmt.Fprintf(&body, `  k%[1]d: project(fullPath: $p%[1]d) @include(if: $q%[1]d) {
    issue(iid: $n%[1]d) @include(if: $i%[1]d) { title state webUrl }
    mergeRequest(iid: $n%[1]d) @include(if: $m%[1]d) { title state webUrl }
    labels(title: $t%[1]d, includeAncestorGroups: true, first: 1) @include(if: $l%[1]d) { nodes { title color textColor } }
    milestones(title: $t%[1]d, includeAncestors: true, first: 1) @include(if: $ms%[1]d) { nodes { title webPath } }
    snippets(ids: [$s%[1]d], first: 1) @include(if: $sn%[1]d) { nodes { title webUrl } }
  }
  v%[1]d: user(username: $t%[1]d) @include(if: $u%[1]d) { name webUrl }
  w%[1]d: group(fullPath: $p%[1]d) @include(if: $g%[1]d) { name webUrl epic: workItem(iid: $n%[1]d) @include(if: $e%[1]d) { title state webUrl } }
`, i)
	}
	return forgeapi.NewDocument(forgeapi.ForgeGitLab, fmt.Sprintf("query ForgeFacts%d(%s) {\n%s}", slots, vars.String(), body.String()))
}

// gitlabFactsQueries are the facts documents by slot count.
var gitlabFactsQueries = factSlots{
	{4, gitlabFactsQuery(4)},
	{16, gitlabFactsQuery(16)},
	{32, gitlabFactsQuery(32)},
}

// gitlabNoSnippet is the snippet id an unused slot sends: GitLab parses
// every SnippetID variable, used or not.
const gitlabNoSnippet = "gid://gitlab/ProjectSnippet/0"

// gitlabFactVars are the variables of a slots document reading keys.
func gitlabFactVars(slots int, keys []forgeFactKey) map[string]any {
	vars := make(map[string]any, 13*slots)
	for i := range slots {
		n := strconv.Itoa(i)
		path, number, title, snippet := "", "", "", gitlabNoSnippet
		on := map[string]bool{}
		if i < len(keys) {
			k := keys[i]
			path, title = k.namespace, k.name
			if k.number > 0 {
				number = strconv.Itoa(k.number)
			}
			switch k.kind {
			case "ref":
				on["q"], on["i"] = true, true
			case "mr":
				on["q"], on["m"] = true, true
			case "label":
				on["q"], on["l"] = true, true
			case "milestone":
				on["q"], on["ms"] = true, true
			case "snippet":
				on["q"], on["sn"] = true, true
				snippet = "gid://gitlab/ProjectSnippet/" + number
			case "epic":
				on["g"], on["e"] = true, true
			case "user":
				path = k.name
				on["u"], on["g"] = true, true
			}
		}
		vars["p"+n], vars["n"+n], vars["t"+n], vars["s"+n] = path, number, title, snippet
		for _, flag := range []string{"q", "i", "m", "l", "ms", "sn", "u", "g", "e"} {
			vars[flag+n] = on[flag]
		}
	}
	return vars
}

type gitlabFactItemRaw struct {
	Title  string  `json:"title"`
	State  string  `json:"state"`
	WebURL *string `json:"webUrl"`
}

type gitlabFactProjectRaw struct {
	Issue        *gitlabFactItemRaw `json:"issue"`
	MergeRequest *gitlabFactItemRaw `json:"mergeRequest"`
	Labels       *struct {
		Nodes []struct {
			Title     string `json:"title"`
			Color     string `json:"color"`
			TextColor string `json:"textColor"`
		} `json:"nodes"`
	} `json:"labels"`
	Milestones *struct {
		Nodes []struct {
			Title   string `json:"title"`
			WebPath string `json:"webPath"`
		} `json:"nodes"`
	} `json:"milestones"`
	Snippets *struct {
		Nodes []struct {
			Title  string `json:"title"`
			WebURL string `json:"webUrl"`
		} `json:"nodes"`
	} `json:"snippets"`
}

type gitlabFactAccountRaw struct {
	Name   *string            `json:"name"`
	WebURL string             `json:"webUrl"`
	Epic   *gitlabFactItemRaw `json:"epic"`
}

func (f *gitlabForge) resolveFacts(ctx context.Context, pr PRReference, keys []forgeFactKey) []ForgeFact {
	client, err := f.core.gitlabAPI(pr.Host)
	if err != nil {
		out := make([]ForgeFact, len(keys))
		for i, k := range keys {
			out[i] = unavailableFact(k.key)
		}
		return out
	}
	return f.core.cachedForgeFacts(ctx, f.facts, client, pr, keys, f.sendFacts)
}

func (f *gitlabForge) sendFacts(ctx context.Context, client *forgeapi.Client, pr PRReference, keys []forgeFactKey) ([]ForgeFact, string) {
	return sendFactSlots(ctx, pr, gitlabFactsQueries, keys, gitlabFactsTooCostly, func(ctx context.Context, doc forgeapi.Document, slots int, keys []forgeFactKey) ([]ForgeFact, *forgeapi.Response, error) {
		var data map[string]json.RawMessage
		resp, err := client.GraphQL(ctx, doc, gitlabFactVars(slots, keys), &data)
		answer, err := newFactAnswer(data, err)
		if err != nil {
			return nil, resp, err
		}
		out := make([]ForgeFact, len(keys))
		unavailable := 0
		for i, k := range keys {
			out[i] = gitlabFact(pr.Host, answer, i, k)
			if out[i].Status == FactUnavailable {
				unavailable++
			}
		}
		logFactProblems(pr, doc.Operation(), answer, unavailable)
		return out, resp, nil
	})
}

// gitlabFactsTooCostly reports GitLab's refusal of a document over its
// complexity limit ("Query has complexity of N, which exceeds max
// complexity of 250"), which a smaller read avoids.
func gitlabFactsTooCostly(err error) bool {
	if gql, ok := errors.AsType[*forgeapi.GraphQLError](err); ok {
		for _, problem := range gql.Problems {
			if strings.Contains(strings.ToLower(problem.Message), "complexity") {
				return true
			}
		}
	}
	return githubTooLarge(err)
}

// gitlabFact is slot i's answer for k on host.
func gitlabFact(host string, answer factAnswer, i int, k forgeFactKey) ForgeFact {
	n := strconv.Itoa(i)
	found := ForgeFact{Key: k.key, Status: FactFound}
	switch k.kind {
	case "user":
		if status := answer.status("v"+n, "w"+n); status == FactUnavailable {
			return unavailableFact(k.key)
		}
		var user, group gitlabFactAccountRaw
		okUser, err1 := answer.decode("v"+n, &user)
		okGroup, err2 := answer.decode("w"+n, &group)
		switch {
		case err1 != nil || err2 != nil:
			return unavailableFact(k.key)
		case okUser:
			found.Kind, found.URL = "user", user.WebURL
			if user.Name != nil {
				found.Title = factTitle(*user.Name)
			}
		case okGroup:
			found.Kind, found.URL = "group", group.WebURL
			if group.Name != nil {
				found.Title = factTitle(*group.Name)
			}
		default:
			return absentFact(k.key)
		}
		return found
	case "epic":
		if status := answer.status("w" + n); status != "" {
			return ForgeFact{Key: k.key, Status: status}
		}
		var group gitlabFactAccountRaw
		ok, err := answer.decode("w"+n, &group)
		switch {
		case err != nil:
			return unavailableFact(k.key)
		case !ok || group.Epic == nil:
			return absentFact(k.key)
		}
		found.Title, found.State = factTitle(group.Epic.Title), factState(group.Epic.State)
		found.URL = "https://" + host + "/groups/" + k.namespace + "/-/epics/" + strconv.Itoa(k.number)
		if group.Epic.WebURL != nil && *group.Epic.WebURL != "" {
			found.URL = *group.Epic.WebURL
		}
		return found
	}
	if status := answer.status("k" + n); status != "" {
		return ForgeFact{Key: k.key, Status: status}
	}
	var project gitlabFactProjectRaw
	ok, err := answer.decode("k"+n, &project)
	if err != nil {
		return unavailableFact(k.key)
	}
	if !ok {
		return absentFact(k.key)
	}
	item := func(raw *gitlabFactItemRaw, kind string) ForgeFact {
		if raw == nil {
			return absentFact(k.key)
		}
		found.Kind, found.Title, found.State = kind, factTitle(raw.Title), factState(raw.State)
		if raw.WebURL != nil {
			found.URL = *raw.WebURL
		}
		return found
	}
	switch k.kind {
	case "ref":
		return item(project.Issue, "issue")
	case "mr":
		return item(project.MergeRequest, "")
	case "label":
		if project.Labels == nil || len(project.Labels.Nodes) == 0 {
			return absentFact(k.key)
		}
		label := project.Labels.Nodes[0]
		found.Title = factTitle(label.Title)
		found.Color, found.TextColor = factColor(label.Color), factColor(label.TextColor)
		found.URL = "https://" + host + "/" + k.namespace + "/-/issues?label_name=" + url.QueryEscape(label.Title)
		return found
	case "milestone":
		if project.Milestones == nil || len(project.Milestones.Nodes) == 0 {
			return absentFact(k.key)
		}
		milestone := project.Milestones.Nodes[0]
		found.Title = factTitle(milestone.Title)
		if strings.HasPrefix(milestone.WebPath, "/") {
			found.URL = "https://" + host + milestone.WebPath
		}
		return found
	case "snippet":
		if project.Snippets == nil || len(project.Snippets.Nodes) == 0 {
			return absentFact(k.key)
		}
		snippet := project.Snippets.Nodes[0]
		found.Title, found.URL = factTitle(snippet.Title), snippet.WebURL
		return found
	}
	return unavailableFact(k.key)
}

// factColor is a label color as "#rrggbb" (GitLab may answer "#RGB" or
// upper case), empty for anything else.
func factColor(color string) string {
	c := strings.ToLower(strings.TrimSpace(color))
	if len(c) == 4 && c[0] == '#' {
		c = "#" + strings.Repeat(c[1:2], 2) + strings.Repeat(c[2:3], 2) + strings.Repeat(c[3:4], 2)
	}
	if len(c) != 7 || c[0] != '#' {
		return ""
	}
	for _, r := range c[1:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return ""
		}
	}
	return c
}

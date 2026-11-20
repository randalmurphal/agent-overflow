package forgefake

import (
	"fmt"
	"strings"
	"testing"
)

// ghFactVars are ForgeFacts<slots>'s variables with every slot off.
func ghFactVars(slots int) map[string]any {
	vars := map[string]any{}
	for i := range slots {
		n := fmt.Sprint(i)
		vars["o"+n], vars["n"+n], vars["x"+n], vars["r"+n], vars["u"+n] = "", "", 0, false, false
	}
	return vars
}

func glabFactVars(slots int) map[string]any {
	vars := map[string]any{}
	for i := range slots {
		n := fmt.Sprint(i)
		vars["p"+n], vars["n"+n], vars["t"+n], vars["s"+n] = "", "", "", "gid://gitlab/ProjectSnippet/0"
		for _, flag := range []string{"q", "i", "m", "l", "ms", "sn", "u", "g", "e"} {
			vars[flag+n] = false
		}
	}
	return vars
}

// GitHub's facts reads answer each used slot from the fixture, a missing
// repository or item as a NOT_FOUND at its alias and a missing owner as
// null; an unused slot answers nothing; a slot count the app has no
// document for is unhandled.
func TestGitHubFactsRoutes(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := e.Seed(Fixture{
		Accounts: []Account{{Forge: "github", Login: "octo", Name: "Octo"}, {Forge: "github", Login: "acme", Kind: "org"}},
		Repos: []Repo{{Forge: "github", Project: "acme/app", Issues: []Issue{{Number: 1, Title: "One", State: "closed"}},
			Pulls: []Pull{{Number: 2, Title: "Two", Author: "octo", HeadRef: "two", HeadSHA: strings.Repeat("2", 40)}}}},
	}); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	vars := ghFactVars(8)
	vars["o0"], vars["n0"], vars["x0"], vars["r0"] = "acme", "app", 1, true
	vars["o1"], vars["n1"], vars["x1"], vars["r1"] = "acme", "app", 2, true
	vars["o2"], vars["n2"], vars["x2"], vars["r2"] = "acme", "app", 3, true
	vars["o3"], vars["n3"], vars["x3"], vars["r3"] = "gone", "app", 1, true
	vars["o4"], vars["u4"] = "octo", true
	vars["o5"], vars["u5"] = "acme", true
	vars["o6"], vars["u6"] = "ghost", true
	var out struct {
		Data   map[string]map[string]any `json:"data"`
		Errors []struct {
			Type string   `json:"type"`
			Path []string `json:"path"`
		} `json:"errors"`
	}
	decodeBody(t, graphQL(t, srv, "github.com", "ForgeFacts8", vars), &out)
	if got := out.Data["r0"]["issueOrPullRequest"].(map[string]any); got["__typename"] != "Issue" || got["state"] != "CLOSED" || got["title"] != "One" {
		t.Fatalf("r0 = %v", got)
	}
	if got := out.Data["r1"]["issueOrPullRequest"].(map[string]any); got["__typename"] != "PullRequest" || got["prState"] != "OPEN" {
		t.Fatalf("r1 = %v", got)
	}
	if out.Data["r2"]["issueOrPullRequest"] != nil || out.Data["r3"] != nil || out.Data["u6"] != nil {
		t.Fatalf("missing items = %v %v %v", out.Data["r2"], out.Data["r3"], out.Data["u6"])
	}
	if out.Data["u4"]["__typename"] != "User" || out.Data["u4"]["name"] != "Octo" || out.Data["u5"]["__typename"] != "Organization" || out.Data["u5"]["name"] != nil {
		t.Fatalf("owners = %v %v", out.Data["u4"], out.Data["u5"])
	}
	if _, used := out.Data["r7"]; used {
		t.Fatal("an unused slot answered")
	}
	if len(out.Errors) != 2 || out.Errors[0].Path[0] != "r2" || len(out.Errors[0].Path) != 2 || out.Errors[1].Path[0] != "r3" || out.Errors[1].Type != "NOT_FOUND" {
		t.Fatalf("errors = %+v", out.Errors)
	}
	if got := lastInvocation(t, e); got.Route != "gh graphql ForgeFacts8" || got.Unhandled {
		t.Fatalf("invocation = %+v", got)
	}
	nine := ghFactVars(9)
	var params []string
	for name := range nine {
		params = append(params, "$"+name+": String")
	}
	httpRequest(t, srv, "POST", "github.com", "/github/graphql", mustJSON(t, map[string]any{
		"query": testDocument("query", "ForgeFacts9", "("+strings.Join(params, ", ")+")"), "operationName": "ForgeFacts9", "variables": nine,
	}), nil)
	if got := lastInvocation(t, e); !got.Unhandled {
		t.Fatalf("ForgeFacts9 = %+v, want unhandled", got)
	}
}

// GitLab's facts reads answer each used slot's parts, a missing node as
// null or no node.
func TestGitLabFactsRoutes(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := e.Seed(Fixture{
		Accounts: []Account{{Forge: "gitlab", Login: "alice", Name: "Alice"}, {Forge: "gitlab", Login: "grp", Kind: "group", Epics: []Issue{{Number: 3, Title: "Epic"}}}},
		Repos: []Repo{{Forge: "gitlab", Project: "grp/app", Issues: []Issue{{Number: 1, Title: "One"}},
			Labels: []Label{{Name: "bug", Color: "#000000"}}, Milestones: []Milestone{{Title: "v1", Group: "grp"}}, Snippets: []Snippet{{ID: 4, Title: "Snip"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	vars := glabFactVars(4)
	vars["p0"], vars["n0"], vars["t0"], vars["s0"] = "grp/app", "1", "bug", "gid://gitlab/ProjectSnippet/4"
	for _, flag := range []string{"q", "i", "m", "l", "ms", "sn"} {
		vars[flag+"0"] = true
	}
	vars["t0"] = "bug"
	vars["p1"], vars["t1"], vars["u1"], vars["g1"] = "alice", "alice", true, true
	vars["p2"], vars["n2"], vars["g2"], vars["e2"] = "grp", "3", true, true
	vars["p3"], vars["n3"], vars["q3"], vars["i3"] = "gone/app", "1", true, true
	out := glabGraphQL(t, srv, "ForgeFacts4", "query", vars)
	data := out["data"].(map[string]any)
	k0 := data["k0"].(map[string]any)
	if k0["issue"].(map[string]any)["state"] != "opened" || k0["mergeRequest"] != nil ||
		len(k0["labels"].(map[string]any)["nodes"].([]any)) != 1 || len(k0["milestones"].(map[string]any)["nodes"].([]any)) != 0 ||
		len(k0["snippets"].(map[string]any)["nodes"].([]any)) != 1 {
		t.Fatalf("k0 = %v", k0)
	}
	if data["v1"].(map[string]any)["name"] != "Alice" || data["w1"] != nil {
		t.Fatalf("user slot = %v %v", data["v1"], data["w1"])
	}
	if epic := data["w2"].(map[string]any)["epic"].(map[string]any); epic["state"] != "OPEN" || epic["title"] != "Epic" {
		t.Fatalf("epic = %v", epic)
	}
	if data["k3"] != nil {
		t.Fatalf("a missing project = %v", data["k3"])
	}
	// The milestone title "bug" matches none; "v1" matches the group's.
	vars["t0"] = "v1"
	k0 = glabGraphQL(t, srv, "ForgeFacts4", "query", vars)["data"].(map[string]any)["k0"].(map[string]any)
	if nodes := k0["milestones"].(map[string]any)["nodes"].([]any); len(nodes) != 1 || !strings.HasPrefix(nodes[0].(map[string]any)["webPath"].(string), "/groups/grp/-/milestones/") {
		t.Fatalf("group milestone = %v", nodes)
	}
}

// A fixture refuses accounts and facts its forge cannot have.
func TestFactsFixtureValidation(t *testing.T) {
	t.Parallel()
	for name, fixture := range map[string]Fixture{
		"org on gitlab":        {Accounts: []Account{{Forge: "gitlab", Login: "x", Kind: "org"}}},
		"epics on a user":      {Accounts: []Account{{Forge: "gitlab", Login: "x", Epics: []Issue{{Number: 1}}}}},
		"no login":             {Accounts: []Account{{Forge: "github"}}},
		"issue number of pull": {Repos: []Repo{{Forge: "github", Project: "a/b", Issues: []Issue{{Number: 1}}, Pulls: []Pull{{Number: 1, Title: "t", Author: "a", HeadRef: "h", HeadSHA: strings.Repeat("1", 40)}}}}},
		"milestone on github":  {Repos: []Repo{{Forge: "github", Project: "a/b", Milestones: []Milestone{{Title: "v1"}}}}},
		"bad issue state":      {Repos: []Repo{{Forge: "gitlab", Project: "a/b", Issues: []Issue{{Number: 1, State: "merged"}}}}},
	} {
		if _, err := New(Options{APIToken: testAPIToken}).Seed(fixture); err == nil {
			t.Errorf("%s: seeded", name)
		}
	}
}

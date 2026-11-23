package git

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitHub facts: ref: and user: keys. GitHub has no field that reads
// issues by a list of numbers, so each key is an aliased slot: rN, a
// repository's issueOrPullRequest(number:), or uN, repositoryOwner(login:),
// which answers users and organizations. A document is fixed text, so
// one is built at init per slot count (githubFactsQueries) and a read
// sends the smallest that holds its keys, every slot's parts behind
// @include: an unused slot sends false and runs nothing. A missing
// repository, issue or pull answers null beside a NOT_FOUND error at its
// alias, which is that key's absent; a missing owner answers null.

// githubFactsQuery is the facts document with slots slots.
func githubFactsQuery(slots int) forgeapi.Document {
	var vars, body strings.Builder
	for i := range slots {
		if i > 0 {
			vars.WriteString(", ")
		}
		fmt.Fprintf(&vars, "$o%[1]d: String!, $n%[1]d: String!, $x%[1]d: Int!, $r%[1]d: Boolean!, $u%[1]d: Boolean!", i)
		fmt.Fprintf(&body, "  r%[1]d: repository(owner: $o%[1]d, name: $n%[1]d) @include(if: $r%[1]d) { issueOrPullRequest(number: $x%[1]d) { __typename ... on Issue { title state url } ... on PullRequest { title prState: state url } } }\n", i)
		fmt.Fprintf(&body, "  u%[1]d: repositoryOwner(login: $o%[1]d) @include(if: $u%[1]d) { __typename login url ... on User { name } ... on Organization { name } }\n", i)
	}
	return forgeapi.NewDocument(forgeapi.ForgeGitHub, fmt.Sprintf("query ForgeFacts%d(%s) {\n%s}", slots, vars.String(), body.String()))
}

// githubFactsQueries are the facts documents by slot count. 128 keys of
// one call are one request; a call's keys past 128 (at most 256) are a
// second.
var githubFactsQueries = factSlots{
	{8, githubFactsQuery(8)},
	{32, githubFactsQuery(32)},
	{128, githubFactsQuery(128)},
}

// githubFactVars are the variables of a slots document reading keys: a
// key's slot names its repository and number or its login, and every
// other slot is switched off.
func githubFactVars(slots int, keys []forgeFactKey) map[string]any {
	vars := make(map[string]any, 5*slots)
	for i := range slots {
		owner, name, number, ref, user := "", "", 0, false, false
		if i < len(keys) {
			switch k := keys[i]; k.kind {
			case "ref":
				owner, name, _ = strings.Cut(k.namespace, "/")
				number, ref = k.number, true
			case "user":
				owner, user = k.name, true
			}
		}
		n := fmt.Sprint(i)
		vars["o"+n], vars["n"+n], vars["x"+n], vars["r"+n], vars["u"+n] = owner, name, number, ref, user
	}
	return vars
}

type githubFactRepoRaw struct {
	Item *struct {
		Typename string `json:"__typename"`
		Title    string `json:"title"`
		State    string `json:"state"`
		PRState  string `json:"prState"`
		URL      string `json:"url"`
	} `json:"issueOrPullRequest"`
}

type githubFactOwnerRaw struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
	URL      string `json:"url"`
	Name     string `json:"name"`
}

func (f *githubForge) resolveFacts(ctx context.Context, pr PRReference, keys []forgeFactKey, fresh bool) []ForgeFact {
	client, err := f.core.githubAPI(pr.Host)
	if err != nil {
		out := make([]ForgeFact, len(keys))
		for i, k := range keys {
			out[i] = unavailableFact(k.key)
		}
		return out
	}
	return f.core.cachedForgeFacts(ctx, f.facts, client, pr, keys, fresh, f.sendFacts)
}

func (f *githubForge) sendFacts(ctx context.Context, client *forgeapi.Client, pr PRReference, keys []forgeFactKey) ([]ForgeFact, string) {
	return sendFactSlots(ctx, pr, githubFactsQueries, keys, githubTooLarge, func(ctx context.Context, doc forgeapi.Document, slots int, keys []forgeFactKey) ([]ForgeFact, *forgeapi.Response, error) {
		var data map[string]json.RawMessage
		resp, err := client.GraphQL(ctx, doc, githubFactVars(slots, keys), &data)
		answer, err := newFactAnswer(data, err)
		if err != nil {
			return nil, resp, err
		}
		out := make([]ForgeFact, len(keys))
		unavailable := 0
		for i, k := range keys {
			out[i] = githubFact(answer, i, k)
			if out[i].Status == FactUnavailable {
				unavailable++
			}
		}
		logFactProblems(pr, doc.Operation(), answer, unavailable)
		return out, resp, nil
	})
}

// githubFact is slot i's answer for k.
func githubFact(answer factAnswer, i int, k forgeFactKey) ForgeFact {
	alias := fmt.Sprintf("u%d", i)
	if k.kind == "ref" {
		alias = fmt.Sprintf("r%d", i)
	}
	if status := answer.status(alias); status != "" {
		return ForgeFact{Key: k.key, Status: status}
	}
	switch k.kind {
	case "ref":
		var repo githubFactRepoRaw
		ok, err := answer.decode(alias, &repo)
		switch {
		case err != nil:
			return unavailableFact(k.key)
		case !ok || repo.Item == nil:
			return absentFact(k.key)
		}
		fact := ForgeFact{Key: k.key, Status: FactFound, URL: repo.Item.URL, Title: factTitle(repo.Item.Title)}
		switch repo.Item.Typename {
		case "Issue":
			fact.Kind, fact.State = "issue", factState(repo.Item.State)
		case "PullRequest":
			fact.Kind, fact.State = "pull", factState(repo.Item.PRState)
		default:
			return unavailableFact(k.key)
		}
		return fact
	default:
		var owner githubFactOwnerRaw
		ok, err := answer.decode(alias, &owner)
		switch {
		case err != nil:
			return unavailableFact(k.key)
		case !ok:
			return absentFact(k.key)
		}
		fact := ForgeFact{Key: k.key, Status: FactFound, URL: owner.URL, Title: factTitle(owner.Name)}
		switch owner.Typename {
		case "User":
			fact.Kind = "user"
		case "Organization":
			fact.Kind = "org"
		default:
			return unavailableFact(k.key)
		}
		return fact
	}
}

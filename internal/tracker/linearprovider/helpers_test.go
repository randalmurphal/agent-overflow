package linearprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	web = "team-WEB" // lineartest.NewTeam("WEB").ID
	app = "team-APP"
	at  = "2026-11-02T10:00:00.000Z"
)

// issueID is the nth invented issue ID.
func issueID(n int) string { return fmt.Sprintf("6f1c2a3b-0000-4000-8000-%012d", n) }

// newProvider is a Provider over a client of a fake expecting script.
func newProvider(t *testing.T, script ...lineartest.Exchange) (*Provider, *lineartest.Server) {
	t.Helper()
	srv := lineartest.NewServer(t, script...)
	return providerOf(t, srv), srv
}

// providerOf is a new client and Provider on srv, as a restarted process
// would build them.
func providerOf(t *testing.T, srv *lineartest.Server) *Provider {
	t.Helper()
	c, err := linear.New(linear.Options{
		BaseURL: srv.URL(), Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte(lineartest.Key)))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// page is a one-page answer of nodes.
func page(nodes ...string) string { return lineartest.Data(lineartest.PageJSON(nodes, false, "")) }

// nodePage is a one-page answer of a connection under an entity.
func nodePage(nodes ...string) string {
	return `{"data":{` + lineartest.NodePageJSON(nodes, false, "") + `}}`
}

func js(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Exchanges of the catalog reads, as the client sends them.

func teamsX(teams ...lineartest.Team) lineartest.Exchange {
	var nodes []string
	for _, t := range teams {
		nodes = append(nodes, t.JSON())
	}
	return lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, Body: page(nodes...)}
}

func statesX(team string, states ...lineartest.State) lineartest.Exchange {
	var nodes []string
	for _, s := range states {
		nodes = append(nodes, s.JSON())
	}
	return lineartest.Exchange{Op: "WorkflowStates", Vars: `{"teamId":"` + team + `","first":100}`, Body: page(nodes...)}
}

func labelsX(team string, labels ...lineartest.Label) lineartest.Exchange {
	var nodes []string
	for _, l := range labels {
		nodes = append(nodes, l.JSON())
	}
	return lineartest.Exchange{Op: "Labels", Body: page(nodes...),
		Vars: `{"first":100,"filter":{"or":[{"team":{"id":{"eq":"` + team + `"}}},{"team":{"null":true}}]}}`}
}

// idsVars are the variables of a read by the IDs ids.
func idsVars(ids ...string) string { return js(map[string]any{"ids": ids, "first": len(ids)}) }

// byIDX expects a read by ids of op and answers nodes.
func byIDX(op string, ids []string, nodes ...string) lineartest.Exchange {
	return lineartest.Exchange{Op: op, Vars: idsVars(ids...), Body: page(nodes...)}
}

// labelsByIDX expects the read of the labels ids names.
func labelsByIDX(ids []string, labels ...lineartest.Label) lineartest.Exchange {
	var nodes []string
	for _, l := range labels {
		nodes = append(nodes, l.JSON())
	}
	return byIDX("LabelsByID", ids, nodes...)
}

// teamX expects the read of one team.
func teamX(t lineartest.Team) lineartest.Exchange {
	return lineartest.Exchange{Op: "Team", Vars: `{"id":"` + t.ID + `"}`, Body: `{"data":{"team":` + t.JSON() + `}}`}
}

// onBoardX expects the read of ids within team web, answered with
// issues.
func onBoardX(ids []string, issues ...lineartest.Issue) lineartest.Exchange {
	return positionsX(byIDVars(ids, []string{web}, false), false, "", issues...)
}

func membersX(team string, users ...lineartest.User) lineartest.Exchange {
	var nodes []string
	for _, u := range users {
		nodes = append(nodes, u.JSON())
	}
	return lineartest.Exchange{Op: "TeamMembers", Vars: `{"teamId":"` + team + `","first":100}`, Body: nodePage(nodes...)}
}

// keptCyclesX expects the read of the cycles of team that window keeps.
func keptCyclesX(team string, window string, cycles ...lineartest.Cycle) lineartest.Exchange {
	var nodes []string
	for _, c := range cycles {
		nodes = append(nodes, c.JSON())
	}
	since := map[string]string{"gte": window}
	vars := js(map[string]any{"first": 20, "filter": map[string]any{"team": map[string]any{"id": map[string]string{"eq": team}},
		"or": []any{map[string]any{"isActive": map[string]bool{"eq": true}}, map[string]any{"isFuture": map[string]bool{"eq": true}},
			map[string]any{"endsAt": since}, map[string]any{"completedAt": since}}}})
	return lineartest.Exchange{Op: "Cycles", Vars: vars, Body: page(nodes...)}
}

func summaryX(id string, issue lineartest.Issue) lineartest.Exchange {
	return lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"` + id + `"}`, Body: `{"data":{"issue":` + issue.JSON() + `}}`}
}

// byIDVars are the variables of a read of ids, within teams when given.
func byIDVars(ids []string, teams []string, archived bool) string {
	and := []any{map[string]any{"id": map[string]any{"in": ids}}}
	if len(teams) > 0 {
		and = append(and, map[string]any{"team": map[string]any{"id": map[string]any{"in": teams}}})
	}
	v := map[string]any{"first": len(ids), "sort": []any{map[string]any{"manual": map[string]string{"order": "Ascending"}}},
		"filter": map[string]any{"and": and}}
	if archived {
		v["includeArchived"] = true
	}
	return js(v)
}

func issuesX(vars string, issues ...lineartest.Issue) lineartest.Exchange {
	var nodes []string
	for _, is := range issues {
		nodes = append(nodes, is.JSON())
	}
	return lineartest.Exchange{Op: "Issues", Vars: vars, Body: page(nodes...)}
}

// issue is an invented issue of team web with no estimate, labels or
// project.
func issue(n int, sortOrder float64) lineartest.Issue {
	is := lineartest.NewIssue(issueID(n), fmt.Sprintf("WEB-%d", n), at, sortOrder)
	is.Team, is.Estimate, is.LabelIDs, is.Project, is.Number = web, nil, nil, "", n
	return is
}

// updateX expects one update of id with input and answers with answer.
func updateX(id, input, answer string) lineartest.Exchange {
	return lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"` + id + `","input":` + input + `}`, Body: answer}
}

// checkResult fails the test for a result no provider may return.
func checkResult(t *testing.T, r tracker.Result) {
	t.Helper()
	if err := r.Check(); err != nil {
		t.Fatalf("result %s fails Check: %v", describe(r), err)
	}
}

var reasonNames = map[tracker.Reason]string{
	tracker.ReasonTracker: "tracker", tracker.ReasonInvalid: "invalid", tracker.ReasonUnsupported: "unsupported",
	tracker.ReasonStale: "stale", tracker.ReasonLoss: "loss", tracker.ReasonNoMatch: "no match",
	tracker.ReasonAmbiguous: "ambiguous", tracker.ReasonMissing: "missing",
}

// describe is a result's parts as n:step:state(reason) for messages and
// comparisons, n the number of the issue issueID made, "-" for none.
func describe(r tracker.Result) string {
	var parts []string
	for _, p := range r.Parts {
		n := string(p.Issue)
		switch {
		case n == "":
			n = "-"
		case strings.HasPrefix(n, issueID(0)[:24]):
			n = strings.TrimLeft(n[24:], "0")
		}
		s := fmt.Sprintf("%s:%s:%s", n, p.Step, p.Outcome.State())
		if ref, ok := p.Outcome.Refusal(); ok {
			s += "(" + reasonNames[ref.Reason] + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// expectResult fails the test unless r passes Check and its parts read
// want.
func expectResult(t *testing.T, r tracker.Result, want string) {
	t.Helper()
	checkResult(t, r)
	if got := describe(r); got != want {
		t.Errorf("result %s, want %s", got, want)
	}
}

func f64(v float64) *float64 { return &v }

// issueStatesX expects the read of issue id, in state, with team web's
// states.
func issueStatesX(id, state string, states ...lineartest.State) lineartest.Exchange {
	var nodes []string
	for _, s := range states {
		nodes = append(nodes, s.JSON())
	}
	return lineartest.Exchange{Op: "IssueStates", Vars: `{"id":"` + id + `"}`, Body: lineartest.Data(`"issue":{"id":"` + id + `","state":{"id":"` + state + `"},` +
		`"team":{"id":"` + web + `","page":` + lineartest.ConnectionJSON(nodes, false, "") + `}}`)}
}

// dateAt is an answer header naming t as Linear's clock; noDate is one
// that names none.
func dateAt(t time.Time) func() http.Header {
	return func() http.Header { return http.Header{"Date": {t.UTC().Format(http.TimeFormat)}} }
}

func noDate() http.Header { return http.Header{"Date": nil} }

// notFound is Linear's answer to a read of an issue that does not exist.
const notFound = `{"data":null,"errors":[{"message":"Entity not found: Issue","path":["issue"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`

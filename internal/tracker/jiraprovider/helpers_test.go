package jiraprovider

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
)

// The site every test runs against: example.atlassian.net with projects
// WEB (10000) and APP (10001), scrum boards 3 (story points) and 4
// (original time), both ranked by customfield_10019, and users User 1
// (the credential's, in New York) and User 2.

const (
	testToken = "fake-token-jp7"

	fieldPath      = "/rest/api/3/field"
	myselfPath     = "/rest/api/3/myself"
	board3Config   = "/rest/agile/1.0/board/3/configuration"
	board4Config   = "/rest/agile/1.0/board/4/configuration"
	board3Issues   = "/rest/software/1.0/board/3/issue"
	board4Issues   = "/rest/software/1.0/board/4/issue"
	bulkFetchPath  = "/rest/api/3/issue/bulkfetch"
	changelogsPath = "/rest/api/3/changelog/bulkfetch"
	searchPath     = "/rest/api/3/search/jql"

	user1JSON = `{"accountId":"acc-user1","displayName":"User 1","emailAddress":"user1@example.com","active":true,"accountType":"atlassian","timeZone":"America/New_York"}`
	user2JSON = `{"accountId":"acc-user2","displayName":"User 2","emailAddress":"user2@example.com","active":true,"accountType":"atlassian","timeZone":"Europe/Berlin"}`
)

// fieldsJSON is the site's field catalog.
const fieldsJSON = `[
{"id":"summary","name":"Summary","schema":{"type":"string","system":"summary"}},
{"id":"status","name":"Status","schema":{"type":"status","system":"status"}},
{"id":"issuetype","name":"Issue Type","schema":{"type":"issuetype","system":"issuetype"}},
{"id":"priority","name":"Priority","schema":{"type":"priority","system":"priority"}},
{"id":"resolution","name":"Resolution","schema":{"type":"resolution","system":"resolution"}},
{"id":"assignee","name":"Assignee","schema":{"type":"user","system":"assignee"}},
{"id":"reporter","name":"Reporter","schema":{"type":"user","system":"reporter"}},
{"id":"labels","name":"Labels","schema":{"type":"array","items":"string","system":"labels"}},
{"id":"parent","name":"Parent","schema":{"type":"issuelink","system":"parent"}},
{"id":"project","name":"Project","schema":{"type":"project","system":"project"}},
{"id":"created","name":"Created","schema":{"type":"datetime","system":"created"}},
{"id":"updated","name":"Updated","schema":{"type":"datetime","system":"updated"}},
{"id":"resolutiondate","name":"Resolved","schema":{"type":"datetime","system":"resolutiondate"}},
{"id":"description","name":"Description","schema":{"type":"string","system":"description"}},
{"id":"duedate","name":"Due date","schema":{"type":"date","system":"duedate"}},
{"id":"components","name":"Components","schema":{"type":"array","items":"component","system":"components"}},
{"id":"timeoriginalestimate","name":"Original estimate","schema":{"type":"number","system":"timeoriginalestimate"}},
{"id":"customfield_10020","name":"Sprint","schema":{"type":"array","items":"json","custom":"com.pyxis.greenhopper.jira:gh-sprint","customId":10020}},
{"id":"customfield_10019","name":"Rank","schema":{"type":"any","custom":"com.pyxis.greenhopper.jira:gh-lexo-rank","customId":10019}},
{"id":"customfield_10016","name":"Story points","schema":{"type":"number","custom":"com.atlassian.jira.plugin.system.customfieldtypes:float","customId":10016}},
{"id":"customfield_10040","name":"Notes","schema":{"type":"string","custom":"com.atlassian.jira.plugin.system.customfieldtypes:textarea","customId":10040}},
{"id":"customfield_10050","name":"Severity","schema":{"type":"option","custom":"com.atlassian.jira.plugin.system.customfieldtypes:select","customId":10050}},
{"id":"customfield_10060","name":"Reviewers","schema":{"type":"array","items":"user","custom":"com.atlassian.jira.plugin.system.customfieldtypes:multiuserpicker","customId":10060}},
{"id":"customfield_10070","name":"Region","schema":{"type":"option-with-child","custom":"com.atlassian.jira.plugin.system.customfieldtypes:cascadingselect","customId":10070}}
]`

// boardConfig is the configuration of a scrum board ranked by
// customfield_10019 and estimated by field.
func boardConfig(id int, field string) string {
	return boardConfigIn(id, field, `{"type":"project","id":"10000","key":"WEB","name":"Web","self":"x"}`)
}

// boardConfigIn is boardConfig located at location.
func boardConfigIn(id int, field, location string) string {
	return fmt.Sprintf(`{"id":%d,"name":"Board %d","type":"scrum","location":%s,"filter":{"id":"1%d"},
"columnConfig":{"columns":[{"name":"To Do","statuses":[{"id":"10000"}]},{"name":"In Progress","statuses":[{"id":"10001"}]},{"name":"Done","statuses":[{"id":"10002"}]}],"constraintType":"issueCount"},
"estimation":{"type":"field","field":{"fieldId":%q,"displayName":"Estimate"}},
"ranking":{"rankCustomFieldId":10019}}`, id, id, location, id, field)
}

// newSite is a fake Jira serving the catalog, the user and the two
// boards' configurations, with a Provider and its client.
func newSite(t *testing.T) (*jiratest.Server, *Provider) {
	t.Helper()
	f := jiratest.NewServer(t)
	siteRoutes(f)
	return f, newProvider(t, f, nil)
}

// siteRoutes serves on f the catalog, the user and the two boards'
// configurations.
func siteRoutes(f *jiratest.Server) {
	f.On("GET", fieldPath, jiratest.Reply(200, fieldsJSON))
	f.On("GET", myselfPath, jiratest.Reply(200, user1JSON))
	f.On("GET", board3Config, jiratest.Reply(200, boardConfig(3, "customfield_10016")))
	f.On("GET", board4Config, jiratest.Reply(200, boardConfig(4, "timeoriginalestimate")))
}

func newJira(t *testing.T, f *jiratest.Server) *jira.Client {
	t.Helper()
	c, err := jira.New(jira.Options{
		BaseURL: f.URL(), Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Basic("user1@example.com", secret.New([]byte(testToken)))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func newProvider(t *testing.T, f *jiratest.Server, tc *tempo.Client) *Provider {
	t.Helper()
	p, err := New(Options{Jira: newJira(t, f), Tempo: tc, Site: "https://example.atlassian.net"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// issue is an invented issue's JSON: In Progress, a Story of WEB assigned
// to User 1, ranked by key, in sprint 7, with 3 points and two hours of
// original estimate, updated at updated; fields replace or add fields.
func issue(id, key, updated string, fields map[string]any) map[string]any {
	base := map[string]any{
		"summary":              "Issue " + key,
		"status":               statusJSON("10001", "In Progress", "indeterminate"),
		"issuetype":            map[string]any{"id": "10002", "name": "Story", "hierarchyLevel": 0},
		"project":              map[string]any{"id": "10000", "key": "WEB", "name": "Web"},
		"assignee":             json.RawMessage(user1JSON),
		"priority":             map[string]any{"id": "3", "name": "Medium"},
		"labels":               []string{"ui"},
		"created":              "2026-11-01T09:00:00.000+0000",
		"updated":              updated,
		"resolutiondate":       nil,
		"customfield_10019":    "0|i000" + key + ":",
		"customfield_10020":    []map[string]any{{"id": 7, "name": "Sprint 1", "state": "active", "boardId": 3}},
		"customfield_10016":    3,
		"timeoriginalestimate": 7200,
	}
	maps.Copy(base, fields)
	return map[string]any{"id": id, "key": key, "fields": base}
}

func statusJSON(id, name, category string) map[string]any {
	return map[string]any{"id": id, "name": name, "statusCategory": map[string]any{"key": category}}
}

// page is an issue list or search page: issues, and next naming the
// page after it.
func page(next string, issues ...map[string]any) string {
	v := map[string]any{"issues": issues, "isLast": next == ""}
	if next != "" {
		v["nextPageToken"] = next
	}
	return mustJSON(v)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// bodyOf decodes a request body.
func bodyOf(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("request body %s: %v", b, err)
	}
	return v
}

// checked fails t unless r passes tracker's check, and returns it.
func checked(t *testing.T, r tracker.Result) tracker.Result {
	t.Helper()
	if err := r.Check(); err != nil {
		t.Fatalf("result %+v: %v", r, err)
	}
	return r
}

// states are the states of r's parts, in order.
func states(r tracker.Result) string {
	parts := make([]string, len(r.Parts))
	for i, p := range r.Parts {
		parts[i] = p.Step.String() + ":" + p.Outcome.State().String()
	}
	return strings.Join(parts, ", ")
}

// reason is the refusal reason of r's part i; zero when it is not
// refused.
func reason(r tracker.Result, i int) tracker.Reason {
	ref, _ := r.Parts[i].Outcome.Refusal()
	return ref.Reason
}

func ptr[T any](v T) *T { return &v }

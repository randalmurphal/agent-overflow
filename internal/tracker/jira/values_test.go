package jira

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

func meta(id, name string, s Schema, allowed ...AllowedValue) FieldMeta {
	return FieldMeta{ID: id, Name: name, Schema: s, Operations: []string{"set"}, Allowed: allowed}
}

func av(id, name string, children ...AllowedValue) AllowedValue {
	return AllowedValue{ID: id, Name: name, Children: children}
}

const customPrefix = "com.atlassian.jira.plugin.system.customfieldtypes:"

var (
	textField     = meta("customfield_10002", "Notes line", Schema{Type: "string", Custom: customPrefix + "textfield"})
	summaryField  = meta("summary", "Summary", Schema{Type: "string", System: "summary"})
	pointsField   = meta("customfield_10001", "Story Points", Schema{Type: "number", Custom: customPrefix + "float"})
	dueField      = meta("duedate", "Due date", Schema{Type: "date", System: "duedate"})
	whenField     = meta("customfield_10009", "Deadline", Schema{Type: "datetime", Custom: customPrefix + "datetime"})
	labelsField   = meta("labels", "Labels", Schema{Type: "array", Items: "string", System: "labels"})
	stringsField  = meta("customfield_10013", "Tags", Schema{Type: "array", Items: "string", Custom: "example.app:tags"})
	colorField    = meta("customfield_10003", "Color", Schema{Type: "option", Custom: customPrefix + "select"}, av("100", "Red"), av("101", "red"), av("102", "Blue"), AllowedValue{ID: "103", Name: "Gone", Disabled: true})
	letterField   = meta("customfield_10004", "Letters", Schema{Type: "array", Items: "option", Custom: customPrefix + "multiselect"}, av("110", "A"), av("111", "B"))
	cascadeField  = meta("customfield_10005", "Device", Schema{Type: "option-with-child", Custom: customPrefix + "cascadingselect"}, av("200", "Hardware", av("201", "Laptop"), av("202", "Phone")))
	priorityField = meta("priority", "Priority", Schema{Type: "priority", System: "priority"}, av("1", "High"), av("2", "Low"))
	versionsField = meta("fixVersions", "Fix versions", Schema{Type: "array", Items: "version", System: "fixVersions"}, av("30", "1.0"), av("31", "2.0"))
	parentField   = meta("parent", "Parent", Schema{Type: "issuelink", System: "parent"})
	sprintField   = meta("customfield_10010", "Sprint", Schema{Type: "array", Items: "json", Custom: "com.pyxis.greenhopper.jira:gh-sprint"})
	teamField     = meta("customfield_10014", "Team", Schema{Type: "team", Custom: customPrefix + "atlassian-team"})
	ownerField    = meta("customfield_10006", "Owner", Schema{Type: "user", Custom: customPrefix + "userpicker"})
	reviewersFld  = meta("customfield_10007", "Reviewers", Schema{Type: "array", Items: "user", Custom: customPrefix + "multiuserpicker"})
	unknownField  = meta("customfield_10015", "Account", Schema{Type: "option2", Custom: "example.app:account"})
)

// TestValueEveryKind builds one value of every kind and checks the JSON
// sent and the display.
func TestValueEveryKind(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/user/search", userSearch(t, nil, map[string][]string{"User 1": {userOneJSON}, "User 2": {userTwoJSON}}))
	f.on("GET", "/rest/api/3/issue/WEB-100", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "fields", "summary")
		reply(200, `{"id":"10000","key":"WEB-100","fields":{"summary":"Login epic"}}`)(w, r, b)
	})
	for _, tc := range []struct {
		f       FieldMeta
		in      Input
		json    string
		display string
	}{
		{textField, Text("hello"), `"hello"`, "hello"},
		{summaryField, Text("Fix it"), `"Fix it"`, "Fix it"},
		{pointsField, Text(" 3.50 "), `3.5`, "3.5"},
		{dueField, Text("2026-11-20"), `"2026-11-20"`, "2026-11-20"},
		{whenField, Text("2026-11-20T14:30:00+01:00"), `"2026-11-20T14:30:00.000+0100"`, "2026-11-20T14:30:00+01:00"},
		{labelsField, Texts("frontend", "urgent"), `["frontend","urgent"]`, "frontend, urgent"},
		{labelsField, Text("solo"), `["solo"]`, "solo"},
		{stringsField, Texts("x y"), `["x y"]`, "x y"},
		{colorField, Text("Blue"), `{"id":"102"}`, "Blue"},
		{colorField, Text("blue"), `{"id":"102"}`, "Blue"},
		{colorField, Text("Red"), `{"id":"100"}`, "Red"},
		{colorField, Text("101"), `{"id":"101"}`, "red"},
		{letterField, Texts("A", "b"), `[{"id":"110"},{"id":"111"}]`, "A, B"},
		{cascadeField, Texts("Hardware", "Phone"), `{"id":"200","child":{"id":"202"}}`, "Hardware / Phone"},
		{cascadeField, Text("Hardware"), `{"id":"200"}`, "Hardware"},
		{priorityField, Text("high"), `{"id":"1"}`, "High"},
		{versionsField, Texts("2.0", "1.0"), `[{"id":"31"},{"id":"30"}]`, "2.0, 1.0"},
		{parentField, Text("WEB-100"), `{"key":"WEB-100"}`, "WEB-100 Login epic"},
		{sprintField, Text("42"), `42`, "sprint 42"},
		{teamField, Text("team-uuid-1"), `"team-uuid-1"`, "team team-uuid-1"},
		{ownerField, Text("User 1"), `{"accountId":"acc-user1"}`, "User 1"},
		{reviewersFld, Texts("User 1", "User 2"), `[{"accountId":"acc-user1"},{"accountId":"acc-user2"}]`, "User 1, User 2"},
		{unknownField, RawJSON(json.RawMessage(`{"id":7}`)), `{"id":7}`, `{"id":7}`},
		{textField, RawJSON(json.RawMessage(`"raw"`)), `"raw"`, `"raw"`},
	} {
		v, err := c.Value(t.Context(), tc.f, tc.in, ValueOptions{})
		if err != nil {
			t.Errorf("%s %v: %v", tc.f.Name, tc.in.texts, err)
			continue
		}
		if v.Update != nil || v.Display != tc.display || v.FieldID != tc.f.ID {
			t.Errorf("%s: update %s display %q", tc.f.Name, v.Update, v.Display)
		}
		wantJSON(t, v.Fields, tc.json)
	}
}

func TestValueClear(t *testing.T) {
	c := newTestClient(t, newFake(t))
	for _, tc := range []struct {
		f              FieldMeta
		fields, update string
	}{
		{textField, `null`, ``},
		{labelsField, `[]`, ``},
		{reviewersFld, `[]`, ``},
		{versionsField, `[]`, ``},
		{parentField, ``, `[{"set":{"none":true}}]`},
		{unknownField, `null`, ``},
	} {
		v, err := c.Value(t.Context(), tc.f, Clear(), ValueOptions{})
		if err != nil || string(v.Fields) != tc.fields || string(v.Update) != tc.update {
			t.Errorf("%s: fields %s update %s err %v", tc.f.Name, v.Fields, v.Update, err)
		}
	}
	required := summaryField
	required.Required = true
	if _, err := c.Value(t.Context(), required, Clear(), ValueOptions{}); err == nil {
		t.Error("a required field was cleared")
	}
}

// TestValueRefusals proves every check that fails before sending, each
// as a *trackerapi.NotSentError.
func TestValueRefusals(t *testing.T) {
	c := newTestClient(t, newFake(t))
	addOnly := labelsField
	addOnly.Operations = []string{"add"}
	for name, tc := range map[string]struct {
		f  FieldMeta
		in Input
	}{
		"no input":                 {textField, Input{}},
		"empty text":               {textField, Text("  ")},
		"no texts":                 {labelsField, Texts()},
		"an empty text among many": {labelsField, Texts("a", "")},
		"several on single":        {textField, Texts("a", "b")},
		"three on cascade":         {cascadeField, Texts("Hardware", "Phone", "x")},
		"not a number":             {pointsField, Text("three")},
		"infinite number":          {pointsField, Text("Inf")},
		"not a date":               {dueField, Text("tomorrow")},
		"impossible date":          {dueField, Text("2026-02-30")},
		"date-time without zone":   {whenField, Text("2026-11-20 14:30")},
		"label with a space":       {labelsField, Texts("two words")},
		"disabled option":          {colorField, Text("Gone")},
		"bad child":                {cascadeField, Texts("Hardware", "Desk")},
		"sprint by name":           {sprintField, Text("Sprint 42")},
		"unknown type as text":     {unknownField, Text("Main account")},
		"raw that is not JSON":     {textField, RawJSON(json.RawMessage(`{`))},
		"set not offered":          {addOnly, Texts("a")},
		"EditedFrom on plain text": {textField, Text("x").EditedFrom(layoutDoc())},
		"parent that is no key":    {parentField, Text("not a key")},
	} {
		_, err := c.Value(t.Context(), tc.f, tc.in, ValueOptions{})
		if _, ok := errors.AsType[*trackerapi.NotSentError](err); !ok {
			t.Errorf("%s: got %v, want a refusal before sending", name, err)
		}
	}
}

func TestValueMatchErrors(t *testing.T) {
	c := newTestClient(t, newFake(t))
	_, err := c.Value(t.Context(), colorField, Text("RED"), ValueOptions{})
	me, ok := errors.AsType[*MatchError](err)
	if !ok || !me.Ambiguous || len(me.Candidates) != 2 {
		t.Errorf("ambiguous: %v", err)
	}
	_, err = c.Value(t.Context(), colorField, Text("Green"), ValueOptions{})
	me, ok = errors.AsType[*MatchError](err)
	if !ok || me.Ambiguous || len(me.Candidates) != 3 || !strings.Contains(err.Error(), "Blue") {
		t.Errorf("missing: %v (disabled options are not candidates)", err)
	}
}

func TestValuePriorityWithoutAllowedValues(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/priority/search", reply(200, `{"values":[{"id":"1","name":"High"},{"id":"2","name":"Low"}],"isLast":true}`))
	bare := meta("priority", "Priority", Schema{Type: "priority", System: "priority"})
	v, err := c.Value(t.Context(), bare, Text("Low"), ValueOptions{})
	if err != nil || string(v.Fields) != `{"id":"2"}` {
		t.Fatalf("got %s %v", v.Fields, err)
	}
}

func TestValueNamesUncheckedWithoutAllowedValues(t *testing.T) {
	c := newTestClient(t, newFake(t))
	bare := meta("components", "Components", Schema{Type: "array", Items: "component", System: "components"})
	v, err := c.Value(t.Context(), bare, Texts("Web", "API"), ValueOptions{})
	if err != nil || len(v.Warnings) != 1 {
		t.Fatalf("got %v %v", v, err)
	}
	wantJSON(t, v.Fields, `[{"name":"Web"},{"name":"API"}]`)
}

// TestValueAssigneeMustBeAssignable proves the assignee is checked with
// the assignable endpoint by account ID however the user was named: by
// name, by account ID, or from the cache.
func TestValueAssigneeMustBeAssignable(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/user/search", userSearch(t, map[string]string{"acc-user1": userOneJSON}, map[string][]string{"User 2": {userTwoJSON}}))
	// Only User 2 can be assigned. The check must ask by account ID: the
	// fake refuses a request without one, as Jira does.
	search := assignableSearch(t, map[string]string{"acc-user2": userTwoJSON}, nil)
	f.on("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "issueKey", "WEB-1")
		if !r.URL.Query().Has("accountId") {
			t.Error("the scoped check does not ask by account ID")
		}
		search(w, r, b)
	})
	assignee := meta("assignee", "Assignee", Schema{Type: "user", System: "assignee"})
	v, err := c.Value(t.Context(), assignee, Text("User 2"), ValueOptions{Issue: "WEB-1"})
	if err != nil || string(v.Fields) != `{"accountId":"acc-user2"}` {
		t.Fatalf("got %s %v", v.Fields, err)
	}
	for _, who := range []string{"acc-user1", "acc-user1"} { // the second from the cache
		if _, err := c.Value(t.Context(), assignee, Text(who), ValueOptions{Issue: "WEB-1"}); !trackerapi.WriteNotApplied(err) {
			t.Errorf("a user who cannot be assigned: %v", err)
		}
	}
	if n := f.count("GET", "/rest/api/3/user/assignable/search"); n != 3 {
		t.Errorf("%d scoped checks, want 3", n)
	}
	// An answer listing only someone else does not vouch for the user.
	f.on("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "issueKey", "WEB-1")
		wantQuery(t, r, "accountId", "acc-user1")
		reply(200, "["+userTwoJSON+"]")(w, r, b)
	})
	if _, err := c.Value(t.Context(), assignee, Text("acc-user1"), ValueOptions{Issue: "WEB-1"}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("an answer naming another user: %v", err)
	}
}

func TestValueParentMissing(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-999", reply(404, `{"errorMessages":["Issue does not exist"]}`))
	f.on("GET", myselfURL, reply(200, userOneJSON))
	_, err := c.Value(t.Context(), parentField, Text("WEB-999"), ValueOptions{})
	if !errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestKindOfEverySchema(t *testing.T) {
	for _, tc := range []struct {
		s    Schema
		want kind
	}{
		{Schema{Type: "string", System: "description"}, kindRich},
		{Schema{Type: "string", System: "environment"}, kindRich},
		{Schema{Type: "string", Custom: customPrefix + "textarea"}, kindRich},
		{Schema{Type: "string", Custom: customPrefix + "url"}, kindText},
		{Schema{Type: "number"}, kindNumber},
		{Schema{Type: "date"}, kindDate},
		{Schema{Type: "datetime"}, kindDateTime},
		{Schema{Type: "user"}, kindUser},
		{Schema{Type: "array", Items: "user"}, kindUsers},
		{Schema{Type: "array", Items: "string", Custom: customPrefix + "labels"}, kindLabels},
		{Schema{Type: "array", Items: "string"}, kindStrings},
		{Schema{Type: "option"}, kindOption},
		{Schema{Type: "array", Items: "option"}, kindOptions},
		{Schema{Type: "option-with-child"}, kindCascade},
		{Schema{Type: "resolution"}, kindNamed},
		{Schema{Type: "issuetype"}, kindNamed},
		{Schema{Type: "securitylevel"}, kindNamed},
		{Schema{Type: "version"}, kindNamed},
		{Schema{Type: "array", Items: "component"}, kindNamedList},
		{Schema{Type: "issuelink"}, kindIssue},
		{Schema{Type: "array", Items: "json", Custom: "com.pyxis.greenhopper.jira:gh-sprint"}, kindSprint},
		{Schema{Type: "team"}, kindTeam},
		{Schema{Type: "array", Items: "issuelinks", System: "issuelinks"}, kindUnknown},
		{Schema{Type: "any"}, kindUnknown},
	} {
		if got := kindOf(tc.s); got != tc.want {
			t.Errorf("%+v: kind %d, want %d", tc.s, got, tc.want)
		}
		if _, ok := rules[tc.want]; !ok {
			t.Errorf("kind %d has no rule", tc.want)
		}
	}
}

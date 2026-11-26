package jiraprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jira/jiratest"
)

func TestKindOf(t *testing.T) {
	custom := func(typ, items, kind string) jira.Schema {
		return jira.Schema{Type: typ, Items: items, Custom: "com.atlassian.jira.plugin.system.customfieldtypes:" + kind}
	}
	for _, tc := range []struct {
		schema jira.Schema
		kind   tracker.Kind
		list   bool
	}{
		{jira.Schema{Type: "string", System: "summary"}, tracker.KindText, false},
		{jira.Schema{Type: "string", System: "description"}, tracker.KindRichText, false},
		{jira.Schema{Type: "string", System: "environment"}, tracker.KindRichText, false},
		{custom("string", "", "textarea"), tracker.KindRichText, false},
		{custom("string", "", "textfield"), tracker.KindText, false},
		{custom("number", "", "float"), tracker.KindNumber, false},
		{jira.Schema{Type: "date", System: "duedate"}, tracker.KindDate, false},
		{custom("datetime", "", "datetime"), tracker.KindDateTime, false},
		{jira.Schema{Type: "user", System: "assignee"}, tracker.KindUser, false},
		{custom("array", "user", "multiuserpicker"), tracker.KindUser, true},
		{jira.Schema{Type: "priority", System: "priority"}, tracker.KindOption, false},
		{custom("option", "", "select"), tracker.KindOption, false},
		{custom("array", "option", "multiselect"), tracker.KindOption, true},
		{jira.Schema{Type: "array", Items: "string", System: "labels"}, tracker.KindOption, true},
		{custom("array", "string", "labels"), tracker.KindOption, true},
		{jira.Schema{Type: "array", Items: "component", System: "components"}, tracker.KindOption, true},
		{jira.Schema{Type: "array", Items: "version", System: "fixVersions"}, tracker.KindOption, true},
		{jira.Schema{Type: "array", Items: "json", Custom: sprintType}, tracker.KindReadOnly, false},
		{jira.Schema{Type: "any", Custom: rankType}, tracker.KindReadOnly, false},
		{jira.Schema{Type: "issuelink", System: "parent"}, tracker.KindOption, false},
		{jira.Schema{Type: "issuelink"}, tracker.KindReadOnly, false},
		{custom("option-with-child", "", "cascadingselect"), tracker.KindReadOnly, false},
		{jira.Schema{Type: "team"}, tracker.KindReadOnly, false},
		{jira.Schema{}, tracker.KindReadOnly, false},
	} {
		if kind, list := kindOf(tc.schema); kind != tc.kind || list != tc.list {
			t.Errorf("%+v: %v %v, want %v %v", tc.schema, kind, list, tc.kind, tc.list)
		}
	}
}

// detailIssue is issue 10001 as a read of every field returns it.
func detailIssue() map[string]any {
	notes := adf.Doc(para("some notes"))
	return issue("10001", "WEB-1", updatedAt, map[string]any{
		"description":       adf.Doc(para("The description")),
		"reporter":          json.RawMessage(user2JSON),
		"duedate":           "2026-12-01",
		"components":        []any{map[string]any{"id": "40", "name": "API"}},
		"customfield_10040": notes,
		"customfield_10060": []any{json.RawMessage(user2JSON)},
		"customfield_10070": map[string]any{"id": "30", "value": "North", "child": map[string]any{"id": "31", "value": "East"}},
		"customfield_10050": "Minor", // not an option's shape
		"customfield_10099": map[string]any{"name": "Team A"},
		"issuelinks": []any{map[string]any{
			"id":   "300",
			"type": map[string]any{"id": "10000", "name": "Blocks", "inward": "is blocked by", "outward": "blocks"},
			"outwardIssue": map[string]any{"id": "10002", "key": "WEB-2", "fields": map[string]any{
				"summary": "Other", "status": statusJSON("10002", "Done", "done")}},
		}},
		"attachment": []any{map[string]any{"id": "501", "filename": "a.txt", "mimeType": "text/plain", "size": 5,
			"created": updatedAt, "author": json.RawMessage(user2JSON)}},
	})
}

// commentsJSON answers comment reads from a list of n, by offset, oldest
// first or, for orderBy=-created, newest first.
func commentsJSON(t *testing.T, n int) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		size, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
		order := r.URL.Query().Get("orderBy")
		if order != "created" && order != "-created" {
			t.Errorf("comments ordered by %q", order)
		}
		var out []string
		for i := start; i < min(start+size, n); i++ {
			at := i
			if order == "-created" {
				at = n - 1 - i
			}
			out = append(out, fmt.Sprintf(`{"id":"%d","author":%s,"body":%s,"created":%q,"updated":%q}`,
				900+at, user1JSON, mustJSON(adf.Doc(para(fmt.Sprint("comment ", at)))), updatedAt, updatedAt))
		}
		jiratest.Reply(200, fmt.Sprintf(`{"startAt":%d,"total":%d,"comments":[%s]}`, start, n, strings.Join(out, ",")))(w, r, b)
	}
}

// detailChangelog is issue 10001's one change, as the changelog read
// pages it and as an issue read embeds it.
const detailChange = `{"id":"100","author":` + user2JSON + `,"created":"2026-11-02T09:00:00.000+0000",
	"items":[{"field":"status","fieldId":"status","fieldtype":"jira","from":"10000","fromString":"To Do","to":"10001","toString":"In Progress"}]}`

func detailSite(t *testing.T, comments int) (*jiratest.Server, *Provider) {
	f, p := newSite(t)
	f.On("GET", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		// Every field but the comments and worklogs, which Detail reads
		// or leaves out, with the screen, the moves and the changelog
		// embedded (the server embeds the routes below).
		jiratest.WantQuery(t, r, "fields", "*all,-comment,-worklog")
		jiratest.WantQuery(t, r, "expand", "editmeta,changelog,transitions,transitions.fields")
		is := detailIssue()
		is["changelog"] = json.RawMessage(`{"startAt":0,"maxResults":1,"total":1,"histories":[` + detailChange + `]}`)
		jiratest.Reply(200, mustJSON(is))(w, r, b)
	})
	var fields []string
	for _, id := range []string{"summary", "description", "priority", "customfield_10016"} {
		fields = append(fields, `"`+id+`":`+metaFields[id])
	}
	f.On("GET", editMetaPath, jiratest.Reply(200, `{"fields":{`+strings.Join(fields, ",")+`}}`))
	f.On("GET", "/rest/api/3/issue/10001/comment", commentsJSON(t, comments))
	f.On("GET", "/rest/api/3/issue/10001/changelog", jiratest.Reply(200, `{"startAt":0,"total":1,"isLast":true,"values":[`+detailChange+`]}`))
	f.On("GET", "/rest/api/3/issue/10001/transitions", jiratest.Reply(200, `{"transitions":[
		{"id":"31","name":"Resolve","to":{"id":"10002","name":"Done","statusCategory":{"key":"done"}},"isAvailable":true,"fields":{
		 "resolution":{"key":"resolution","name":"Resolution","required":true,"schema":{"type":"resolution","system":"resolution"},"operations":["set"],"allowedValues":[{"id":"1","name":"Fixed"}]}}},
		{"id":"41","name":"Reopen","to":{"id":"10000","name":"To Do","statusCategory":{"key":"new"}},"isAvailable":false}]}`))
	return f, p
}

func TestDetail(t *testing.T) {
	_, p := detailSite(t, 3)
	d, err := p.Detail(t.Context(), "10001")
	if err != nil {
		t.Fatal(err)
	}
	if u := IssueURL("https://example.atlassian.net/", d.Card.Key); u != "https://example.atlassian.net/browse/WEB-1" || d.Card.URL != "" ||
		d.Description != adf.ToMarkdown(adf.Doc(para("The description"))) {
		t.Errorf("detail %s, card URL %q, %q", u, d.Card.URL, d.Description)
	}
	byID := map[tracker.FieldID]tracker.Field{}
	var ids []string
	for _, f := range d.Fields {
		byID[f.ID] = f
		ids = append(ids, string(f.ID))
	}
	for _, skipped := range []string{"summary", "status", "priority", "assignee", "labels", "description", "customfield_10019", "customfield_10020", "issuelinks", "attachment", "comment"} {
		if slices.Contains(ids, skipped) {
			t.Errorf("field %s is listed", skipped)
		}
	}
	check := func(id tracker.FieldID, kind tracker.Kind, editable bool, value string) {
		t.Helper()
		f, ok := byID[id]
		if !ok {
			t.Errorf("no field %s in %v", id, ids)
			return
		}
		got := strings.Join(f.Value.Texts(), ",")
		for _, c := range f.Value.Choices() {
			got += c.ID + "=" + c.Name + ","
		}
		for _, n := range f.Value.Numbers() {
			got += fmt.Sprint(n)
		}
		for _, d := range f.Value.Dates() {
			got += d.String()
		}
		if f.Kind != kind || f.Editable != editable || got != value {
			t.Errorf("%s: %v editable %v %q, want %v %v %q", id, f.Kind, f.Editable, got, kind, editable, value)
		}
	}
	check("customfield_10016", tracker.KindNumber, true, "3")
	check("reporter", tracker.KindUser, false, "acc-user2=User 2,")
	check("components", tracker.KindOption, false, "40=API,")
	check("duedate", tracker.KindDate, false, "2026-12-01")
	check("customfield_10040", tracker.KindRichText, false, adf.ToMarkdown(adf.Doc(para("some notes"))))
	check("customfield_10060", tracker.KindUser, false, "acc-user2=User 2,")
	check("customfield_10070", tracker.KindReadOnly, false, "North / East")
	check("customfield_10050", tracker.KindReadOnly, false, "Minor")
	check("customfield_10099", tracker.KindReadOnly, false, "Team A")
	check("timeoriginalestimate", tracker.KindNumber, false, "7200")
	if byID["customfield_10099"].Name != "customfield_10099" || byID["customfield_10070"].Name != "Region" {
		t.Errorf("names %q %q", byID["customfield_10099"].Name, byID["customfield_10070"].Name)
	}
	if len(d.Links) != 1 || d.Links[0] != (tracker.Link{ID: "300", Type: tracker.LinkType{ID: "10000", Name: "Blocks", Outward: "blocks", Inward: "is blocked by"},
		Outward: true, Issue: tracker.IssueRef{ID: "10002", Key: "WEB-2", Title: "Other", Status: "10002"}}) {
		t.Errorf("links %+v", d.Links)
	}
	if len(d.Attachments) != 1 || d.Attachments[0].ID != "501" || d.Attachments[0].Author != "acc-user2" {
		t.Errorf("attachments %+v", d.Attachments)
	}
	if len(d.Comments.Comments) != 3 || d.Comments.Comments[0].ID != "902" || d.Comments.Next != "" {
		t.Errorf("comments %+v", d.Comments)
	}
	if len(d.History) != 1 || d.History[0].From != "To Do" || d.History[0].Author != "acc-user2" {
		t.Errorf("history %+v", d.History)
	}
	if len(d.Moves) != 1 || d.Moves[0].ID != "31" || len(d.Moves[0].Fields) != 1 || !d.Moves[0].Fields[0].Required {
		t.Errorf("moves %+v", d.Moves)
	}
	var users, statuses []string
	for _, u := range d.Users {
		users = append(users, string(u.ID))
	}
	for _, s := range d.Statuses {
		statuses = append(statuses, string(s.ID))
	}
	slices.Sort(users)
	slices.Sort(statuses)
	if !slices.Equal(users, []string{"acc-user1", "acc-user2"}) || !slices.Equal(statuses, []string{"10001", "10002"}) {
		t.Errorf("catalog users %v statuses %v", users, statuses)
	}
}

// TestComments proves comments page newest first with no comment
// skipped or repeated, and that a page Jira cut short fails.
func TestComments(t *testing.T) {
	f, p := detailSite(t, 120)
	var got []string
	var token tracker.PageToken
	for range 5 {
		page, err := p.Comments(t.Context(), "10001", token)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Comments {
			got = append(got, string(c.ID))
		}
		if token = page.Next; token == "" {
			break
		}
	}
	if len(got) != 120 || got[0] != "1019" || got[119] != "900" {
		t.Fatalf("got %d comments, %v ... %v", len(got), got[:3], got[len(got)-3:])
	}
	for i := 1; i < len(got); i++ {
		if a, b := mustAtoi(t, got[i]), mustAtoi(t, got[i-1]); a != b-1 {
			t.Fatalf("comment %s after %s", got[i], got[i-1])
		}
	}
	// The newest 50 in one read, then 50 and 20 by offset.
	if n := f.Count("GET", "/rest/api/3/issue/10001/comment"); n != 3 {
		t.Errorf("%d reads, want 3", n)
	}
	for _, bad := range []tracker.PageToken{"x", "0", "-5", "07"} {
		if _, err := p.Comments(t.Context(), "10001", bad); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
	f.On("GET", "/rest/api/3/issue/10001/comment", jiratest.Reply(200, `{"startAt":0,"total":80,"comments":[{"id":"900","body":null,"created":"2026-11-10T10:00:00.000+0000"}]}`))
	if _, err := p.Comments(t.Context(), "10001", ""); err == nil {
		t.Error("a page Jira cut short was taken")
	}
}

func TestMovesAndScreens(t *testing.T) {
	f, p := detailSite(t, 0)
	moves, err := p.Moves(t.Context(), "10001")
	if err != nil || len(moves) != 1 || moves[0].To != "10002" || moves[0].Fields[0].Kind != tracker.KindOption {
		t.Fatalf("moves %+v %v", moves, err)
	}
	f.On("GET", "/rest/api/3/issue/createmeta/10000/issuetypes/10002", jiratest.Reply(200, createMeta(true, "summary", "description", "priority", "labels", "customfield_10050", "customfield_10070", "customfield_10020")))
	fields, err := p.CreateFields(t.Context(), "10000", "10002")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, fd := range fields {
		got = append(got, fmt.Sprintf("%s:%v:%v:%v", fd.ID, fd.Kind, fd.Required, fd.Editable))
	}
	want := []string{
		fmt.Sprintf("description:%v:false:true", tracker.KindRichText),
		fmt.Sprintf("priority:%v:false:true", tracker.KindOption),
		fmt.Sprintf("labels:%v:false:true", tracker.KindOption),
		fmt.Sprintf("customfield_10050:%v:true:true", tracker.KindOption),
		fmt.Sprintf("customfield_10070:%v:false:false", tracker.KindReadOnly),
	}
	if !slices.Equal(got, want) {
		t.Errorf("create fields\n%v\nwant\n%v", got, want)
	}
}

// A create field is required of the person only when Jira would not fill
// it: required with a default (Priority, commonly) is not, as the
// create's planner reads it.
func TestCreateFieldsRequireWhatHasNoDefault(t *testing.T) {
	f, p := detailSite(t, 0)
	fields := []string{
		`{"fieldId":"summary","key":"summary","name":"Summary","required":true,"schema":{"type":"string","system":"summary"},"operations":["set"]}`,
		`{"fieldId":"priority","key":"priority","name":"Priority","required":true,"hasDefaultValue":true,"schema":{"type":"priority","system":"priority"},"operations":["set"]}`,
		`{"fieldId":"customfield_10050","key":"customfield_10050","name":"Team","required":true,"schema":{"type":"option","custom":"com.atlassian.jira.plugin.system.customfieldtypes:select","customId":10050},"operations":["set"],"allowedValues":[{"id":"1","value":"Red"},{"id":"2","value":"Blue","disabled":true}]}`,
	}
	f.On("GET", "/rest/api/3/issue/createmeta/10000/issuetypes/10002", jiratest.Reply(200, `{"total":3,"fields":[`+strings.Join(fields, ",")+`]}`))
	got, err := p.CreateFields(t.Context(), "10000", "10002")
	if err != nil {
		t.Fatal(err)
	}
	required := map[tracker.FieldID]bool{}
	for _, fd := range got {
		required[fd.ID] = fd.Required
	}
	if required["priority"] || !required["customfield_10050"] {
		t.Errorf("required %v, want priority (defaulted) not required and the team required", required)
	}
	// The screen's listed values ride with the field, so its picker reads
	// nothing; a field the screen lists none for has none.
	for _, fd := range got {
		switch fd.ID {
		case "customfield_10050":
			if !slices.Equal(fd.Allowed, []tracker.Choice{{ID: "1", Name: "Red"}}) {
				t.Errorf("team allowed %v", fd.Allowed)
			}
		case "priority":
			if fd.Allowed != nil {
				t.Errorf("priority allowed %v, want none listed", fd.Allowed)
			}
		}
	}
}

func TestChoices(t *testing.T) {
	f, p := detailSite(t, 0)
	f.On("GET", "/rest/api/3/issue/createmeta/10000/issuetypes/10002", jiratest.Reply(200, createMeta(false, "summary", "priority", "customfield_10050")))
	f.On("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "issueId", "10001")
		jiratest.WantQuery(t, r, "query", "user")
		jiratest.WantQuery(t, r, "maxResults", "1000")
		jiratest.Reply(200, `[`+user1JSON+`,`+user2JSON+`]`)(w, r, b)
	})
	f.On("GET", "/rest/api/3/priority/search", jiratest.Reply(200, `{"isLast":true,"values":[{"id":"1","name":"High"},{"id":"4","name":"Low"}]}`))
	onCreate := tracker.OnCreate("10000", "10002")
	sev, err := p.Choices(t.Context(), onCreate, "customfield_10050", " min")
	if err != nil || sev.More || !slices.Equal(sev.Choices, []tracker.Choice{{ID: "20", Name: "Minor"}, {ID: "21", Name: "MINOR"}}) {
		t.Errorf("severity %+v %v", sev, err)
	}
	users, err := p.AssignableUsers(t.Context(), tracker.OnIssue("10001"), "user")
	if err != nil || !users.More || len(users.Users) != 2 || users.Users[1].ID != "acc-user2" {
		t.Errorf("assignable %+v %v", users, err)
	}
	if users, err := p.AssignableUsers(t.Context(), tracker.OnIssue("10001"), " "); err != nil || users.Users != nil || !users.More {
		t.Errorf("no text: %+v %v", users, err)
	}
	if _, err := p.Choices(t.Context(), onCreate, "customfield_10016", ""); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("a field not on the screen: %v", err)
	}
	if got, err := p.Choices(t.Context(), tracker.OnIssue("10001"), "summary", "x"); err != nil || got.Choices != nil || got.More {
		t.Errorf("free text: %v %v", got, err)
	}
	// An edit screen's priority with no allowed values lists the site's.
	f.On("GET", editMetaPath, jiratest.Reply(200, `{"fields":{"priority":{"key":"priority","name":"Priority","schema":{"type":"priority","system":"priority"},"operations":["set"]}}}`))
	if got, err := p.Choices(t.Context(), tracker.OnIssue("10001"), "priority", "l"); err != nil || got.More || !slices.Equal(got.Choices, []tracker.Choice{{ID: "4", Name: "Low"}}) {
		t.Errorf("priorities %v %v", got, err)
	}
}

// TestParentChoices proves a parent's choices are the issues text finds,
// by text and by a key it is shaped like, never the issue edited, each
// by ID named by key and title; empty text sends nothing.
func TestParentChoices(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", editMetaPath, jiratest.Reply(200, `{"fields":{"parent":`+parentMeta+`}}`))
	f.On("GET", "/rest/api/3/issue/createmeta/10000/issuetypes/10002", jiratest.Reply(200, `{"total":1,"fields":[{"fieldId":"parent",`+parentMeta[1:]+`]}`))
	var jqls []string
	f.On("POST", searchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		v := bodyOf(t, b)
		jqls = append(jqls, v["jql"].(string))
		if v["maxResults"] != float64(pickLimit) || fmt.Sprint(v["fields"]) != "[summary]" {
			t.Errorf("search %s", b)
		}
		jiratest.Reply(200, `{"isLast":true,"issues":[{"id":"10005","key":"WEB-5","fields":{"summary":"Epic"}}]}`)(w, r, b)
	})
	got, err := p.Choices(t.Context(), tracker.OnIssue("10001"), tracker.FieldParent, " web-5 ")
	if err != nil || got.More || !slices.Equal(got.Choices, []tracker.Choice{{ID: "10005", Name: "WEB-5 Epic"}}) {
		t.Errorf("choices %+v %v", got, err)
	}
	if _, err := p.Choices(t.Context(), tracker.OnCreate("10000", "10002"), tracker.FieldParent, "login"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"(text ~ " + jira.QuoteJQLText("web-5") + " OR key in " + jira.QuoteJQLList("web-5") + ") AND id != 10001 ORDER BY updated DESC",
		"text ~ " + jira.QuoteJQLText("login") + " ORDER BY updated DESC",
	}
	if !slices.Equal(jqls, want) {
		t.Errorf("sent\n%s", strings.Join(jqls, "\n"))
	}
	if got, err := p.Choices(t.Context(), tracker.OnIssue("10001"), tracker.FieldParent, " "); err != nil || got.Choices != nil || !got.More || len(jqls) != 2 {
		t.Errorf("no text: %+v %v, %d searches", got, err, len(jqls))
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

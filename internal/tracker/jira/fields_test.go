package jira

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/jira/jiratest"
)

const catalogJSON = `[
 {"id":"summary","key":"summary","name":"Summary","custom":false,"schema":{"type":"string","system":"summary"}},
 {"id":"customfield_10001","key":"customfield_10001","name":"Story Points","custom":true,"schema":{"type":"number","custom":"com.atlassian.jira.plugin.system.customfieldtypes:float","customId":10001},"clauseNames":["cf[10001]","Story Points"]}
]`

// The catalog is read once and handed out, uncopied, until it is siteAge
// old, so a field renamed in Jira shows its new name by then.
func TestFieldCatalogIsCachedUntilItAges(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	read := time.Now()
	at := read
	c.catalog.clock = func() time.Time { return at }
	f.On("GET", "/rest/api/3/field", jiratest.Reply(200, catalogJSON))
	var first *Catalog
	for range 2 {
		cat, err := c.Fields(t.Context())
		sp, ok := cat.Field("customfield_10001")
		if err != nil || cat.Len() != 2 || !ok || sp.Schema.CustomID != 10001 || sp.Schema.customType() != "float" {
			t.Fatalf("got %+v %v", cat, err)
		}
		if first != nil && cat != first {
			t.Error("a cached catalog was copied")
		}
		first = cat
	}
	if n := f.Count("GET", "/rest/api/3/field"); n != 1 {
		t.Errorf("%d reads, want 1", n)
	}
	for i, age := range []time.Duration{siteAge - time.Nanosecond, siteAge} {
		at = read.Add(age)
		if _, err := c.Fields(t.Context()); err != nil {
			t.Fatal(err)
		}
		if n := f.Count("GET", "/rest/api/3/field"); n != 1+i {
			t.Errorf("%d reads with the catalog %s old, want %d", n, age, 1+i)
		}
	}
}

func TestCreateMetaPagesAndKeys(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "maxResults", "200")
		if r.URL.Query().Get("startAt") == "0" {
			jiratest.Reply(200, `{"issueTypes":[{"id":"1","name":"Task"}],"total":2}`)(w, r, b)
			return
		}
		jiratest.WantQuery(t, r, "startAt", "1")
		jiratest.Reply(200, `{"createMetaIssueType":[{"id":"2","name":"Bug"}],"total":2}`)(w, r, b)
	})
	f.On("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/1", jiratest.Reply(200, `{"total":2,"results":[
		{"fieldId":"summary","key":"summary","name":"Summary","required":true,"schema":{"type":"string","system":"summary"},"operations":["set"]},
		{"fieldId":"priority","key":"priority","name":"Priority","required":false,"schema":{"type":"priority","system":"priority"},"operations":["set"],
		 "allowedValues":[{"id":"1","name":"High"},{"id":2,"name":"Low"}],"hasDefaultValue":true}]}`))
	types, err := c.CreateIssueTypes(t.Context(), "WEB")
	if err != nil || len(types) != 2 || types[1].Name != "Bug" {
		t.Fatalf("got %+v %v", types, err)
	}
	s, err := c.CreateMeta(t.Context(), "WEB", "1")
	if err != nil || len(s.Fields) != 2 {
		t.Fatalf("got %+v %v", s, err)
	}
	p, err := s.Field("priority")
	if err != nil || !p.HasDefault || p.Allowed[1].ID != "2" || p.Allowed[1].Name != "Low" {
		t.Fatalf("got %+v %v", p, err)
	}
	if _, err := c.CreateMeta(t.Context(), "WEB", "1/../2"); err == nil {
		t.Error("an issue type ID that is not numeric reached a path")
	}
}

func TestCreateMetaEmptyPageStops(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if f.Count("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes") > 10 {
			t.Error("the client kept asking for pages")
			jiratest.Reply(400, `{}`)(w, r, b)
			return
		}
		jiratest.Reply(200, `{"issueTypes":[],"total":3}`)(w, r, b)
	})
	if _, err := c.CreateIssueTypes(t.Context(), "WEB"); err == nil {
		t.Error("an empty page short of the total must fail, not loop")
	}
}

// TestFieldCatalogCopiesShareNothing proves what a catalog hands out is
// a copy: changing it changes nothing the next caller reads.
func TestFieldCatalogCopiesShareNothing(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/field", jiratest.Reply(200, catalogJSON))
	first, err := c.Fields(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := first.Field("customfield_10001")
	sp.Name, sp.Schema.Custom = "changed", "changed"
	for f := range first.All() {
		f.Name = "changed too"
	}
	again, _ := c.Fields(t.Context())
	if sp, _ := again.Field("customfield_10001"); sp.Name != "Story Points" || sp.Schema.Custom == "changed" {
		t.Errorf("a caller's change reached the cache: %+v", sp)
	}
	if got, ok := again.OfCustomType("com.atlassian.jira.plugin.system.customfieldtypes:float"); !ok || got.ID != "customfield_10001" {
		t.Errorf("by custom type: %+v %v", got, ok)
	}
	if _, ok := again.Field("customfield_404"); ok {
		t.Error("found a field the catalog lacks")
	}
}

// TestFieldIDWinsOverName proves a field named by its exact ID is that
// field even when another field carries the ID as its name.
func TestFieldIDWinsOverName(t *testing.T) {
	s := Screen{Fields: []FieldMeta{
		{ID: "customfield_10020", Name: "summary"},
		{ID: "summary", Name: "Summary"},
	}}
	if f, err := s.Field("summary"); err != nil || f.ID != "summary" {
		t.Errorf("got %+v %v", f, err)
	}
	if f, err := s.Field("SUMMARY"); err == nil {
		t.Errorf("two names in another case: %+v", f)
	}
}

func TestEditMetaAndScreenLookup(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/editmeta", jiratest.Reply(200, `{"fields":{
		"customfield_10002":{"key":"customfield_10002","name":"Team","required":false,"schema":{"type":"team","custom":"com.atlassian.jira.plugin.system.customfieldtypes:atlassian-team"},"operations":["set"]},
		"customfield_10003":{"key":"customfield_10003","name":"team","required":false,"schema":{"type":"string"},"operations":["set"]},
		"labels":{"key":"labels","name":"Labels","required":false,"schema":{"type":"array","items":"string","system":"labels"},"operations":["add","set","remove"]}}}`))
	s, err := c.EditMeta(t.Context(), "WEB-1")
	if err != nil || len(s.Fields) != 3 {
		t.Fatalf("got %+v %v", s, err)
	}
	if f, err := s.Field("Team"); err != nil || f.ID != "customfield_10002" {
		t.Errorf("an exact spelling must win: %+v %v", f, err)
	}
	if f, err := s.Field("labels"); err != nil || f.ID != "labels" {
		t.Errorf("by ID: %+v %v", f, err)
	}
	_, err = s.Field("TEAM")
	if me, ok := errors.AsType[*MatchError](err); !ok || !me.Ambiguous {
		t.Errorf("ambiguous: %v", err)
	}
	_, err = s.Field("Sprint")
	if me, ok := errors.AsType[*MatchError](err); !ok || me.Ambiguous || len(me.Candidates) != 3 {
		t.Errorf("missing: %v", err)
	}
}

func TestMatchErrorListsBoundedCandidates(t *testing.T) {
	var cands []Candidate
	for i := range 30 {
		cands = append(cands, Candidate{ID: fmt.Sprint(i), Name: fmt.Sprintf("Option %d", i)})
	}
	_, err := match("Team value", "nothing", cands)
	me, ok := errors.AsType[*MatchError](err)
	if !ok || len(me.Candidates) != 30 {
		t.Fatalf("got %v", err)
	}
	if want := "and 10 more"; !strings.Contains(err.Error(), want) {
		t.Errorf("message %q lacks %q", err, want)
	}
}

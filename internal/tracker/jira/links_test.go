package jira

import (
	"net/http"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

func TestLinkAndUnlinkIssues(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issueLinkType", reply(200, `{"issueLinkTypes":[{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"},{"id":"10001","name":"Relates","inward":"relates to","outward":"relates to"}]}`))
	f.on("POST", "/rest/api/3/issueLink", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"type":{"id":"10000"},"outwardIssue":{"key":"WEB-1"},"inwardIssue":{"id":"10002"}}`)
		w.WriteHeader(201)
	})
	f.on("DELETE", "/rest/api/3/issueLink/601", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	if err := c.LinkIssues(t.Context(), LinkInput{Type: "blocks", Outward: "WEB-1", Inward: "10002"}); err != nil {
		t.Fatal(err)
	}
	if err := c.LinkIssues(t.Context(), LinkInput{Type: "Duplicates", Outward: "WEB-1", Inward: "WEB-2"}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("an unknown type: %v", err)
	}
	if err := c.UnlinkIssues(t.Context(), "601"); err != nil {
		t.Fatal(err)
	}
	if err := c.UnlinkIssues(t.Context(), "6/01"); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a bad link ID: %v", err)
	}
}

func TestRemoteLinks(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1/remotelink", reply(200, `[{"id":10000,"globalId":"system=forge&id=7","relationship":"mentioned in",
		"application":{"name":"Forge"},"object":{"url":"https://forge.example.com/mr/7","title":"MR 7","status":{"resolved":true}}}]`))
	f.on("POST", "/rest/api/3/issue/WEB-1/remotelink", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"object":{"url":"https://docs.example.com/page","title":"Design"},"globalId":"page-1"}`)
		reply(201, `{"id":10001,"self":"x"}`)(w, r, b)
	})
	f.on("DELETE", "/rest/api/3/issue/WEB-1/remotelink/10001", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	links, err := c.RemoteLinks(t.Context(), "WEB-1")
	if err != nil || len(links) != 1 || links[0].ID != "10000" || !links[0].Resolved || links[0].Application != "Forge" {
		t.Fatalf("got %+v %v", links, err)
	}
	id, err := c.AddRemoteLink(t.Context(), "WEB-1", RemoteLinkInput{URL: "https://docs.example.com/page", Title: "Design", GlobalID: "page-1"})
	if err != nil || id != "10001" {
		t.Fatalf("got %q %v", id, err)
	}
	for _, in := range []RemoteLinkInput{{URL: "javascript:alert(1)", Title: "x"}, {URL: "ftp://files.example.com/a", Title: "x"}, {URL: "https://example.com"}} {
		if _, err := c.AddRemoteLink(t.Context(), "WEB-1", in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if err := c.RemoveRemoteLink(t.Context(), "WEB-1", "10001"); err != nil {
		t.Fatal(err)
	}
}

func TestWatchers(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1/watchers", reply(200, `{"isWatching":true,"watchCount":3,"watchers":[`+userOneJSON+`]}`))
	f.on("POST", "/rest/api/3/issue/WEB-1/watchers", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if string(b) != `"acc-user2"` {
			t.Errorf("body %s", b)
		}
		w.WriteHeader(204)
	})
	f.on("DELETE", "/rest/api/3/issue/WEB-1/watchers", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "accountId", "acc-user2")
		w.WriteHeader(204)
	})
	ws, err := c.Watchers(t.Context(), "WEB-1")
	if err != nil || !ws.Watching || ws.Count != 3 || len(ws.Users) != 1 {
		t.Fatalf("got %+v %v", ws, err)
	}
	if err := c.AddWatcher(t.Context(), "WEB-1", "acc-user2"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveWatcher(t.Context(), "WEB-1", "acc-user2"); err != nil {
		t.Fatal(err)
	}
}

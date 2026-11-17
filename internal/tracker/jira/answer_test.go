package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

// escapedToken is testToken written with a JSON escape, which only a
// decoder turns back into the credential.
var escapedToken = strings.Replace(testToken, "k", `k`, 1)

// noToken fails when text holds the credential.
func noToken(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(text, testToken) {
		t.Errorf("%s repeats the credential: %s", what, text)
	}
}

// TestAnswersAreScrubbed proves the credential, as an answer writes it
// plainly or with a JSON escape, reaches no error, problem, warning,
// failure or value the package returns.
func TestAnswersAreScrubbed(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	for _, tok := range []string{testToken, escapedToken} {
		f.on("GET", "/rest/api/3/issue/WEB-1", reply(200, `{"id":"10001","key":"WEB-1","fields":{
			"summary":"`+tok+`","updated":"`+tok+`","customfield_10001":"`+tok+`"}}`))
		is, err := c.GetIssue(t.Context(), "WEB-1")
		if err != nil || len(is.Problems) != 1 {
			t.Fatalf("got %+v %v", is, err)
		}
		noToken(t, "a problem", is.Problems[0].Err.Error())
		noToken(t, "the summary", is.Summary)
		text, _ := is.Field("customfield_10001").Text()
		noToken(t, "a raw field", text+string(is.Field("customfield_10001")))

		f.on("POST", "/rest/api/3/issue/bulkfetch", reply(200, `{"issues":[],"issueErrors":[{"id":"1","errorMessage":"`+tok+`"}]}`))
		res, err := c.BulkFetch(t.Context(), []string{"WEB-1"})
		if err != nil || len(res.Failed) != 1 {
			t.Fatalf("got %+v %v", res, err)
		}
		noToken(t, "a bulk failure", res.Failed[0].Message)

		f.on("POST", "/rest/api/3/search/jql", reply(200, `{"issues":[],"isLast":true,"warnings":[{"type":"X","message":"`+tok+`"}]}`))
		all, err := c.SearchAll(t.Context(), SearchQuery{JQL: "project = WEB"}, 0)
		if err != nil || len(all.Warnings) != 1 {
			t.Fatalf("got %+v %v", all, err)
		}
		noToken(t, "a search warning", all.Warnings[0].Message)

		f.on("GET", "/rest/api/3/issue/WEB-1/editmeta", reply(200, `{"fields":{"`+tok+`":{"name":"`+tok+`","schema":{"type":"string"}}}}`))
		s, err := c.EditMeta(t.Context(), "WEB-1")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Field("Sprint")
		noToken(t, "a match error", fmt.Sprint(err))

		f.on("GET", "/rest/api/3/myself", reply(200, `{"accountId":`+`"`+tok+`"`+`,"displayName":5}`))
		_, err = c.Myself(t.Context())
		if _, ok := errors.AsType[*AnswerError](err); !ok {
			t.Fatalf("an answer of the wrong shape: %v", err)
		}
		noToken(t, "a decode error", err.Error())
	}
}

func TestAnswerThatIsNotJSON(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	// A second value after a scrubbed first must not be dropped silently
	// when the first is written again.
	for _, body := range []string{`<html>` + testToken + `</html>`, `{"accountId":"a"} {"x":1}`, `{"accountId":"` + testToken + `"} {"x":1}`, ``} {
		f.on("GET", "/rest/api/3/myself", reply(200, body))
		_, err := c.Myself(t.Context())
		if _, ok := errors.AsType[*AnswerError](err); !ok {
			t.Errorf("%q: %v", body, err)
		}
		noToken(t, "the error", fmt.Sprint(err))
	}
}

// TestWrittenCommentWithUnreadableAnswer proves a comment Jira stored
// keeps its ID when its answer does not decode, and the error says the
// write happened, for every path that writes a comment.
func TestWrittenCommentWithUnreadableAnswer(t *testing.T) {
	bad := `{"id":"900","body":"not a document"}`
	check := func(name string, cm *Comment, err error) {
		t.Helper()
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepComment || !se.Applied() || se.OutcomeUnknown() || trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
		if cm == nil || cm.ID != "900" {
			t.Errorf("%s: the comment %+v lost its ID", name, cm)
		}
	}
	f, c := commentSite(t, "service_desk")
	f.on("POST", "/rest/api/3/issue/WEB-1/comment", reply(201, bad))
	res, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "x", Audience: AudiencePublic})
	check("AddComment", res.Comment, err)

	f.on("GET", "/rest/api/3/issue/WEB-1/transitions", reply(200, `{"transitions":[{"id":"21","name":"Start","isAvailable":true,"fields":{}}]}`))
	f.on("POST", "/rest/api/3/issue/WEB-1/transitions", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	tres, err := c.Transition(t.Context(), TransitionInput{Issue: "WEB-1", Transition: "Start", Comment: "x", Audience: AudiencePublic})
	check("Transition", tres.Comment, err)

	f.on("GET", "/rest/api/3/issue/WEB-1/comment/900", reply(200, `{"id":"900","body":{"type":"doc","version":1,"content":[]}}`))
	f.on("PUT", "/rest/api/3/issue/WEB-1/comment/900", reply(200, bad))
	eres, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Markdown: "y"})
	check("EditComment", eres.Comment, err)
}

func TestOtherWritesWithUnreadableAnswers(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/api/3/issue/WEB-1/attachments", reply(200, `{"not":"a list"}`))
	_, err := c.Attach(t.Context(), "WEB-1", File{Name: "a.txt", Content: strings.NewReader("x")})
	if se, ok := errors.AsType[*StepError](err); !ok || se.Step != StepUpload || !se.Applied() {
		t.Errorf("Attach: %v", err)
	}
	for _, body := range []string{`{"self":"x"}`, `<html>stored</html>`} {
		f.on("POST", "/rest/api/3/issue/WEB-1/remotelink", reply(201, body))
		_, err = c.AddRemoteLink(t.Context(), "WEB-1", RemoteLinkInput{URL: "https://example.com", Title: "x"})
		if se, ok := errors.AsType[*StepError](err); !ok || se.Step != StepRemoteLink || !se.Applied() {
			t.Errorf("AddRemoteLink answered %s: %v", body, err)
		}
	}
}

// TestUndefinedAudienceIsRefused proves an Audience outside its
// constants is refused before anything is read or sent, for a comment
// and for a transition's comment.
func TestUndefinedAudienceIsRefused(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1/transitions", reply(200, `{"transitions":[{"id":"21","name":"Start","isAvailable":true,"fields":{}}]}`))
	_, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "x", Audience: Audience(99)})
	if !trackerapi.WriteNotApplied(err) {
		t.Errorf("AddComment: %v", err)
	}
	_, err = c.Transition(t.Context(), TransitionInput{Issue: "WEB-1", Transition: "Start", Comment: "x", Audience: Audience(99)})
	if !trackerapi.WriteNotApplied(err) {
		t.Errorf("Transition: %v", err)
	}
	for _, r := range f.requests() {
		if !strings.HasSuffix(r, "/transitions") {
			t.Errorf("sent %s", r)
		}
	}
}

func TestScrubbedBodyKeepsCleanAnswersAsSent(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	const body = `{"z":1,"a":"<b>&","n":12345678901234567890}`
	f.on("GET", "/rest/api/3/field", reply(200, `[]`))
	resp, err := c.call(t.Context(), trackerapi.Request{Path: apiPath + "field"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = []byte(body)
	got, err := scrubbedBody(resp)
	if err != nil || string(got) != body {
		t.Errorf("a clean answer changed: %s %v", got, err)
	}
	resp.Body = []byte(`{"k":"` + testToken + `","n":12345678901234567890,"h":"<&>"}`)
	got, err = scrubbedBody(resp)
	var v map[string]any
	if err != nil || json.Unmarshal(got, &v) != nil || strings.Contains(string(got), testToken) ||
		!strings.Contains(string(got), "12345678901234567890") || v["h"] != "<&>" {
		t.Errorf("scrubbed: %s %v", got, err)
	}
	// Once the credential the request carried is dropped, nothing can be
	// checked for it, and the answer is refused rather than used.
	resp.Body = []byte(`{"summary":"text"}`)
	c.ForgetCredential()
	if _, err := scrubbedBody(resp); !errors.Is(err, errCredentialChanged) {
		t.Errorf("after the credential was dropped: %v", err)
	}
}

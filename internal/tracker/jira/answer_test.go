package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// escapedToken is testToken written with a JSON escape for its "k",
// which only a decoder turns back into the credential. The backslash is
// built at run time, so no edit of this source can turn the escape into
// the letter it stands for.
var escapedToken = strings.Replace(testToken, "k", string(rune(0x5c))+"u006b", 1)

func TestEscapedTokenIsEscaped(t *testing.T) {
	var decoded string
	if err := json.Unmarshal([]byte(`"`+escapedToken+`"`), &decoded); err != nil || decoded != testToken || strings.Contains(escapedToken, testToken) {
		t.Fatalf("escapedToken %q decodes to %q (%v): not an escape of the credential", escapedToken, decoded, err)
	}
}

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
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	for _, tok := range []string{testToken, escapedToken} {
		f.On("GET", "/rest/api/3/issue/WEB-1", jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{
			"summary":"`+tok+`","updated":"`+tok+`","customfield_10001":"`+tok+`"}}`))
		is, err := c.GetIssue(t.Context(), "WEB-1")
		if err != nil || len(is.Problems) != 1 {
			t.Fatalf("got %+v %v", is, err)
		}
		noToken(t, "a problem", is.Problems[0].Err.Error())
		noToken(t, "the summary", is.Summary)
		text, _ := is.Field("customfield_10001").Text()
		noToken(t, "a raw field", text+string(is.Field("customfield_10001")))

		f.On("POST", "/rest/api/3/issue/bulkfetch", jiratest.Reply(200, `{"issues":[],"issueErrors":[{"id":"1","errorMessage":"`+tok+`"}]}`))
		res, err := c.BulkFetch(t.Context(), []string{"WEB-1"})
		if err != nil || len(res.Failed) != 1 {
			t.Fatalf("got %+v %v", res, err)
		}
		noToken(t, "a bulk failure", res.Failed[0].Message)

		f.On("POST", "/rest/api/3/search/jql", jiratest.Reply(200, `{"issues":[],"isLast":true,"warnings":[{"type":"X","message":"`+tok+`"}]}`))
		all, err := c.SearchAll(t.Context(), SearchQuery{JQL: "project = WEB"}, 0)
		if err != nil || len(all.Warnings) != 1 {
			t.Fatalf("got %+v %v", all, err)
		}
		noToken(t, "a search warning", all.Warnings[0].Message)

		f.On("GET", "/rest/api/3/issue/WEB-1/editmeta", jiratest.Reply(200, `{"fields":{"`+tok+`":{"name":"`+tok+`","schema":{"type":"string"}}}}`))
		s, err := c.EditMeta(t.Context(), "WEB-1")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Field("Sprint")
		noToken(t, "a match error", fmt.Sprint(err))

		f.On("GET", "/rest/api/3/myself", jiratest.Reply(200, `{"accountId":`+`"`+tok+`"`+`,"displayName":5}`))
		_, err = c.Myself(t.Context())
		if _, ok := errors.AsType[*AnswerError](err); !ok {
			t.Fatalf("an answer of the wrong shape: %v", err)
		}
		noToken(t, "a decode error", err.Error())
	}
}

func TestAnswerThatIsNotJSON(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	// A second value after a scrubbed first must not be dropped silently
	// when the first is written again.
	for _, body := range []string{`<html>` + testToken + `</html>`, `{"accountId":"a"} {"x":1}`, `{"accountId":"` + testToken + `"} {"x":1}`, ``} {
		f.On("GET", "/rest/api/3/myself", jiratest.Reply(200, body))
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
	f.On("POST", "/rest/api/3/issue/WEB-1/comment", jiratest.Reply(201, bad))
	res, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "x", Audience: AudiencePublic})
	check("AddComment", res.Comment, err)

	f.On("GET", "/rest/api/3/issue/WEB-1/transitions", jiratest.Reply(200, `{"transitions":[{"id":"21","name":"Start","isAvailable":true,"fields":{}}]}`))
	f.On("POST", "/rest/api/3/issue/WEB-1/transitions", func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(204) })
	tres, err := c.Transition(t.Context(), TransitionInput{Issue: "WEB-1", Transition: "Start", Comment: "x", Audience: AudiencePublic})
	check("Transition", tres.Comment, err)

	f.On("GET", "/rest/api/3/issue/WEB-1/comment/900", jiratest.Reply(200, `{"id":"900","body":{"type":"doc","version":1,"content":[]}}`))
	f.On("PUT", "/rest/api/3/issue/WEB-1/comment/900", jiratest.Reply(200, bad))
	eres, err := c.EditComment(t.Context(), CommentEdit{Issue: "WEB-1", ID: "900", Unchecked: true, Markdown: "y"})
	check("EditComment", eres.Comment, err)

	// An answer whose shape fails before the body is read keeps its ID.
	f.On("POST", "/rest/api/3/issue/WEB-1/comment", jiratest.Reply(201, `{"id":"900","author":5,"body":{"type":"doc","version":1,"content":[]}}`))
	res, err = c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "x", Audience: AudiencePublic})
	check("AddComment with a malformed author", res.Comment, err)

	// An ID sent as a number fails the comment's decode; the answer still
	// names the comment, so it is applied and keeps the ID.
	f.On("POST", "/rest/api/3/issue/WEB-1/comment", jiratest.Reply(201, `{"id":900,"body":{"type":"doc","version":1,"content":[]}}`))
	res, err = c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "x", Audience: AudiencePublic})
	check("AddComment with a numeric ID", res.Comment, err)

	// An answer naming no comment, or not JSON, or with a status the call
	// does not document, is an unknown outcome reporting no comment.
	for _, answer := range []struct {
		status int
		body   string
	}{{201, `{"body":{"type":"doc","version":1,"content":[]}}`}, {201, `{"id":"9x"}`}, {201, `<html>saved</html>`}, {200, `{"id":"900"}`}} {
		f.On("POST", "/rest/api/3/issue/WEB-1/comment", func(w http.ResponseWriter, r *http.Request, b []byte) {
			jiratest.WantJSON(t, b, `{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"x"}]}]}}`)
			jiratest.Reply(answer.status, answer.body)(w, r, b)
		})
		res, err = c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "x", Audience: AudiencePublic})
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepComment || se.Applied() || !se.OutcomeUnknown() || res.Comment != nil {
			t.Errorf("%d %s: %+v %v", answer.status, answer.body, res, err)
		}
	}
}

// TestOtherWritesWithUnknownAnswers proves a 2xx that does not name
// what was written, or is not JSON, or has an undocumented status, is an
// unknown outcome reporting nothing stored.
func TestOtherWritesWithUnknownAnswers(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	unknown := func(name string, step Step, err error) {
		t.Helper()
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != step || se.Applied() || !se.OutcomeUnknown() || trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, answer := range []struct {
		status int
		body   string
	}{{200, `{"not":"a list"}`}, {200, `[]`}, {200, `[{"id":"x"}]`}, {201, `[{"id":"501"}]`}, {200, `<html>stored</html>`}} {
		f.On("POST", "/rest/api/3/issue/WEB-1/attachments", jiratest.Reply(answer.status, answer.body))
		got, err := c.Attach(t.Context(), "WEB-1", File{Name: "a.txt", Content: strings.NewReader("x"), Size: 1})
		unknown("Attach answered "+answer.body, StepUpload, err)
		if got != nil {
			t.Errorf("Attach answered %s: reported %+v", answer.body, got)
		}
	}
	for _, answer := range []struct {
		status int
		body   string
	}{{201, `{"self":"x"}`}, {201, `<html>stored</html>`}, {204, ``}, {202, `{"id":10000}`}} {
		f.On("POST", "/rest/api/3/issue/WEB-1/remotelink", jiratest.Reply(answer.status, answer.body))
		id, err := c.AddRemoteLink(t.Context(), "WEB-1", RemoteLinkInput{URL: "https://example.com", Title: "x"})
		unknown("AddRemoteLink answered "+answer.body, StepRemoteLink, err)
		if id != "" {
			t.Errorf("AddRemoteLink answered %s: reported %q", answer.body, id)
		}
	}
	f.On("POST", "/rest/api/3/issue/WEB-1/remotelink", jiratest.Reply(201, `{"id":10000,"self":5}`))
	if id, err := c.AddRemoteLink(t.Context(), "WEB-1", RemoteLinkInput{URL: "https://example.com", Title: "x"}); err != nil || id != "10000" {
		t.Errorf("a link answer naming the link: %q %v", id, err)
	}
}

// TestUndefinedAudienceIsRefused proves an Audience outside its
// constants is refused before anything is read or sent, for a comment
// and for a transition's comment.
func TestUndefinedAudienceIsRefused(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/transitions", jiratest.Reply(200, `{"transitions":[{"id":"21","name":"Start","isAvailable":true,"fields":{}}]}`))
	_, err := c.AddComment(t.Context(), CommentInput{Issue: "WEB-1", Markdown: "x", Audience: Audience(99)})
	if !trackerapi.WriteNotApplied(err) {
		t.Errorf("AddComment: %v", err)
	}
	_, err = c.Transition(t.Context(), TransitionInput{Issue: "WEB-1", Transition: "Start", Comment: "x", Audience: Audience(99)})
	if !trackerapi.WriteNotApplied(err) {
		t.Errorf("Transition: %v", err)
	}
	for _, r := range f.Requests() {
		if !strings.HasSuffix(r, "/transitions") {
			t.Errorf("sent %s", r)
		}
	}
}

package jira

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// pokerCases drive the poker calls against the fake site, whose add-on is
// installed (conformanceSite).
var pokerCases = map[string]func(t *testing.T, f *fakeEnv){
	"PokerInstalled": func(t *testing.T, f *fakeEnv) {
		if !got(f.c.PokerInstalled(f.ctx)).must(t) {
			t.Fatal("the site's add-on is not found")
		}
	},
	"PokerVotes": func(t *testing.T, f *fakeEnv) {
		if r := got(f.c.PokerVotes(f.ctx, "10101")).must(t); r.Revealed || len(r.Votes) != 0 {
			t.Fatalf("a new round reads %+v", r)
		}
	},
	"CastPokerVote": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.CastPokerVote(f.ctx, "10101", 5))
		check(t, f.c.CastPokerVote(f.ctx, "10101", 3))
		r := got(f.c.PokerVotes(f.ctx, "10101")).must(t)
		if len(r.Votes) != 1 || r.Votes[0] != (PokerVote{Account: "acc-user1", Vote: 3}) {
			t.Fatalf("after two votes the round reads %+v", r)
		}
		if is := f.issueOf(t, "WEB-1"); string(is.Values["customfield_10016"]) != "" {
			t.Fatalf("a vote wrote the estimate: %s", is.Values["customfield_10016"])
		}
	},
	"RevealPokerVotes": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.CastPokerVote(f.ctx, "10101", 8))
		check(t, f.c.RevealPokerVotes(f.ctx, "10101"))
		if r := got(f.c.PokerVotes(f.ctx, "10101")).must(t); !r.Revealed || len(r.Votes) != 1 {
			t.Fatalf("after reveal the round reads %+v", r)
		}
	},
	"ClearPokerVotes": func(t *testing.T, f *fakeEnv) {
		check(t, f.c.CastPokerVote(f.ctx, "10101", 8))
		check(t, f.c.ClearPokerVotes(f.ctx, "10101"))
		if r := got(f.c.PokerVotes(f.ctx, "10101")).must(t); r.Revealed || len(r.Votes) != 0 {
			t.Fatalf("after clear the round reads %+v", r)
		}
	},
}

// pokerCalls counts the poker requests the fake answered: the cloud ID,
// the listing, the issue read, the token and the invokes.
func pokerCalls(t *testing.T, f *fakeEnv) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, r := range f.e.Requests(0).Requests {
		name := r.Route
		if name == "POST gateway/api/graphql" {
			var body struct {
				OperationName string `json:"operationName"`
			}
			if err := json.Unmarshal(r.Body, &body); err != nil {
				t.Fatal(err)
			}
			name = body.OperationName
		}
		out[name]++
	}
	return out
}

// TestPokerTokenIsKeptUntilNearItsExpiry: the cloud ID, the listing and
// one issue's token are read once for many calls; the token is signed
// again pokerTokenMargin before its expiresIn, by the client's clock.
func TestPokerTokenIsKeptUntilNearItsExpiry(t *testing.T) {
	f := newFakeEnv(t)
	f.c.pokerTokens.clock = f.clock.Now
	for range 3 {
		got(f.c.PokerVotes(f.ctx, "10101")).must(t)
	}
	check(t, f.c.CastPokerVote(f.ctx, "10101", 2))
	want := map[string]int{"GET _edge/tenant_info": 1, "Ext": 1, "GET rest/api/3/issue/{issue}": 1, "POST rest/internal/2/forge/context/token": 1, "Invoke": 4}
	if calls := pokerCalls(t, f); !sameCounts(calls, want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
	f.clock.Advance(900*time.Second - pokerTokenMargin - time.Millisecond)
	got(f.c.PokerVotes(f.ctx, "10101")).must(t)
	if n := pokerCalls(t, f)["POST rest/internal/2/forge/context/token"]; n != 1 {
		t.Fatalf("a token in use was signed again: %d tokens", n)
	}
	f.clock.Advance(time.Millisecond)
	got(f.c.PokerVotes(f.ctx, "10101")).must(t)
	if n := pokerCalls(t, f)["POST rest/internal/2/forge/context/token"]; n != 2 {
		t.Fatalf("a token at its margin was used: %d tokens", n)
	}
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestPokerTokenTheGatewayRefusesIsDropped: a token the gateway no
// longer takes (here, expired by the site's clock while this computer's
// stood still) fails the call, and the next call signs another.
func TestPokerTokenTheGatewayRefusesIsDropped(t *testing.T) {
	f := newFakeEnv(t)
	f.c.pokerTokens.clock = func() time.Time { return time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC) }
	got(f.c.PokerVotes(f.ctx, "10101")).must(t)
	f.clock.Advance(900 * time.Second)
	_, err := f.c.PokerVotes(f.ctx, "10101")
	if pe, ok := errors.AsType[*PokerError](err); !ok || pe.Step != pokerStepGateway {
		t.Fatalf("an expired token answered %v", err)
	}
	got(f.c.PokerVotes(f.ctx, "10101")).must(t)
	if n := pokerCalls(t, f)["POST rest/internal/2/forge/context/token"]; n != 2 {
		t.Fatalf("%d tokens; the refused one was not dropped", n)
	}
}

// pokerJWT is a token whose payload carries claims.
func pokerJWT(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","kid":"k"}`)) + "." + enc([]byte(claims)) + "." + enc([]byte("signature"))
}

const (
	pokerTestCloud   = "11111111-1111-4111-8111-111111111111"
	pokerTestClaims  = `{"accountId":"acc-user1","contextIds":["ari:cloud:jira:` + pokerTestCloud + `:workspace/22222222-2222-4222-8222-222222222222"]}`
	pokerTestPanel   = `{"id":"ari:cloud:ecosystem::extension/a/b/static/planning-poker-forge-panel","key":"planning-poker-forge-panel","type":"jira:issuePanel","appId":"a","environmentId":"b","installationId":"c","appVersion":"1.0.0"}`
	pokerTestListing = `{"data":{"extensionContexts":[{"id":"x","extensionsByType":[` + pokerTestPanel + `]}]}}`
	pokerTestIssue   = `{"id":"10101","key":"WEB-1","fields":{"project":{"id":"10000","key":"WEB","name":"Web","projectTypeKey":"software"},"issuetype":{"id":"10001","name":"Story"}}}`
	pokerTestRound   = `{"issueId":"10101","isRevealed":false,"votes":[{"id":"1","issueId":"10101","userId":"acc-user2","vote":5,"createdAt":"2026-11-19T09:00:00Z"}]}`
)

// invokeBody is an invoke answer whose poker app answered status and
// body.
func invokeBody(status int, body string) string {
	b, _ := json.Marshal(body)
	return `{"data":{"invokeExtension":{"success":true,"errors":null,"response":{"body":{"status":` + itoa(status) +
		`,"statusText":"` + http.StatusText(status) + `","headers":{},"body":` + string(b) + `}}}}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// pokerServer is a site answering each poker call with the given answers:
// tenant the cloud ID, listing the extension listing, token the context
// token's, and invoke each invoke (handed the request body).
type pokerAnswers struct {
	tenant, listing, token string
	invoke                 func(body []byte) string
}

func pokerServer(t *testing.T, a pokerAnswers) (*Client, *jiratest.Server) {
	t.Helper()
	s := jiratest.NewServer(t)
	s.On("GET", "/_edge/tenant_info", jiratest.Reply(200, a.tenant))
	s.On("GET", "/rest/api/3/issue/10101", jiratest.Reply(200, pokerTestIssue))
	s.On("POST", "/rest/internal/2/forge/context/token", jiratest.Reply(200, a.token))
	s.On("POST", "/gateway/api/graphql", func(w http.ResponseWriter, r *http.Request, body []byte) {
		answer := a.listing
		if strings.Contains(string(body), `"operationName":"Invoke"`) {
			answer = a.invoke(body)
		}
		jiratest.Reply(200, answer)(w, r, body)
	})
	return newTestClient(t, s), s
}

func measuredAnswers() pokerAnswers {
	return pokerAnswers{
		tenant: `{"cloudId":"` + pokerTestCloud + `"}`, listing: pokerTestListing,
		token:  `{"token":"` + pokerJWT(pokerTestClaims) + `","expiresAt":1,"expiresIn":899442,"extensionId":"x"}`,
		invoke: func([]byte) string { return invokeBody(200, pokerTestRound) },
	}
}

// TestPokerReadsTheMeasuredShapes: the measured answers read as a round,
// and the invoke carries the token's claim and the issue's real values.
func TestPokerReadsTheMeasuredShapes(t *testing.T) {
	a := measuredAnswers()
	var sent []byte
	a.invoke = func(body []byte) string { sent = body; return invokeBody(200, pokerTestRound) }
	c, _ := pokerServer(t, a)
	r := got(c.PokerVotes(t.Context(), "10101")).must(t)
	if r.Revealed || len(r.Votes) != 1 || r.Votes[0] != (PokerVote{Account: "acc-user2", Vote: 5}) {
		t.Fatalf("read %+v", r)
	}
	var req struct {
		Variables struct {
			Input struct {
				ContextIDs []string `json:"contextIds"`
				Payload    struct {
					Context struct {
						Extension pokerExtensionData `json:"extension"`
					} `json:"context"`
				} `json:"payload"`
			} `json:"input"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(sent, &req); err != nil {
		t.Fatal(err)
	}
	want := pokerExtensionData{Type: pokerPanelType, Issue: pokerIssue{Key: "WEB-1", ID: "10101", Type: "Story", TypeID: "10001"},
		Project: pokerProject{ID: "10000", Key: "WEB", Type: "software"}}
	if in := req.Variables.Input; len(in.ContextIDs) != 1 || !strings.HasSuffix(in.ContextIDs[0], ":workspace/22222222-2222-4222-8222-222222222222") ||
		in.Payload.Context.Extension != want {
		t.Fatalf("the invoke sent %s", sent)
	}
}

// TestPokerShapeChangesArePokersOwn: an answer of another shape than the
// measured one, at each call, fails as a PokerError naming the call, and
// a write it stops is not refused.
func TestPokerShapeChangesArePokersOwn(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*pokerAnswers)
		step   string
	}{
		"no cloud ID":             {func(a *pokerAnswers) { a.tenant = `{"id":"x"}` }, pokerStepCloud},
		"listing of no contexts":  {func(a *pokerAnswers) { a.listing = `{"data":{"extensionContexts":[]}}` }, pokerStepListing},
		"panel without its app":   {func(a *pokerAnswers) { a.listing = strings.Replace(pokerTestListing, `"appId":"a"`, `"appId":""`, 1) }, pokerStepListing},
		"listing errors":          {func(a *pokerAnswers) { a.listing = `{"errors":[{"message":"Cannot query field"}]}` }, pokerStepListing},
		"token without its claim": {func(a *pokerAnswers) { a.token = `{"token":"` + pokerJWT(`{"accountId":"a"}`) + `","expiresIn":899442}` }, pokerStepToken},
		"token not a JWT":         {func(a *pokerAnswers) { a.token = `{"token":"opaque","expiresIn":899442}` }, pokerStepToken},
		"body as a string": {func(a *pokerAnswers) {
			a.invoke = func([]byte) string {
				return `{"data":{"invokeExtension":{"success":true,"response":{"body":"{}"}}}}`
			}
		}, pokerStepGateway},
		"round without isRevealed": {func(a *pokerAnswers) {
			a.invoke = func([]byte) string { return invokeBody(200, `{"issueId":"10101","votes":[]}`) }
		}, pokerStepApp},
		"vote without its value": {func(a *pokerAnswers) {
			a.invoke = func([]byte) string { return invokeBody(200, `{"issueId":"10101","isRevealed":true,"votes":[{"userId":"a"}]}`) }
		}, pokerStepApp},
	} {
		t.Run(name, func(t *testing.T) {
			a := measuredAnswers()
			c.change(&a)
			client, _ := pokerServer(t, a)
			_, err := client.PokerVotes(t.Context(), "10101")
			pe, ok := errors.AsType[*PokerError](err)
			if !ok || pe.Step != c.step || pe.Refused {
				t.Fatalf("got %v, want a PokerError of %s", err, c.step)
			}
		})
	}
}

// TestPokerAppStatusDecidesTheWrite: the poker app's 4xx refuses a write,
// its 5xx leaves it unknown, and a 2xx applies it.
func TestPokerAppStatusDecidesTheWrite(t *testing.T) {
	for status, refused := range map[int]bool{400: true, 404: true, 500: false, 503: false} {
		a := measuredAnswers()
		a.invoke = func([]byte) string { return invokeBody(status, `{"message":"no"}`) }
		c, _ := pokerServer(t, a)
		err := c.CastPokerVote(t.Context(), "10101", 3)
		pe, ok := errors.AsType[*PokerError](err)
		if !ok || pe.Refused != refused || trackerapi.WriteNotApplied(err) {
			t.Errorf("HTTP %d: got %v, refused %v", status, err, ok && pe.Refused)
		}
	}
	a := measuredAnswers()
	a.invoke = func([]byte) string { return invokeBody(204, "") }
	c, _ := pokerServer(t, a)
	check(t, c.ClearPokerVotes(t.Context(), "10101"))
}

// TestPokerGatewayRefusalHidesTheToken: a gateway refusal that repeats
// the token it was sent fails with the token replaced, drops the token,
// and is refused for a 4xx statusCode.
func TestPokerGatewayRefusalHidesTheToken(t *testing.T) {
	a := measuredAnswers()
	token := pokerJWT(pokerTestClaims)
	a.invoke = func([]byte) string {
		return `{"data":{"invokeExtension":{"success":false,"response":null,"errors":[{"message":"token ` + token +
			` is not valid","extensions":{"errorType":"X","statusCode":401}}]}}}`
	}
	c, s := pokerServer(t, a)
	err := c.CastPokerVote(t.Context(), "10101", 3)
	pe, ok := errors.AsType[*PokerError](err)
	if !ok || !pe.Refused || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), secret.Redacted) {
		t.Fatalf("got %v", err)
	}
	_ = c.CastPokerVote(t.Context(), "10101", 3)
	if n := s.Count("POST", "/rest/internal/2/forge/context/token"); n != 2 {
		t.Fatalf("%d tokens; the refused one was kept", n)
	}
}

// TestPokerAbsentSiteIsKept: a site whose listing names no poker panel
// answers so for every call within siteAge from one listing, and a vote
// there is not sent.
func TestPokerAbsentSiteIsKept(t *testing.T) {
	a := measuredAnswers()
	a.listing = `{"data":{"extensionContexts":[{"id":"x","extensionsByType":[]}]}}`
	c, s := pokerServer(t, a)
	for range 2 {
		if got(c.PokerInstalled(t.Context())).must(t) {
			t.Fatal("an absent add-on reads as installed")
		}
	}
	_, err := c.PokerVotes(t.Context(), "10101")
	if !errors.Is(err, ErrPokerAbsent) {
		t.Fatalf("a read answered %v", err)
	}
	if err := c.CastPokerVote(t.Context(), "10101", 3); !errors.Is(err, ErrPokerAbsent) || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("a vote answered %v", err)
	}
	if n := s.Count("POST", "/gateway/api/graphql"); n != 1 {
		t.Fatalf("%d listings, want 1", n)
	}
}

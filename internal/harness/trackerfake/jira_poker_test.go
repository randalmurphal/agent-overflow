package trackerfake

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const memberToken = "member-token-0002"

// pokerRig is a rig whose site has the poker add-on and a member, User 2,
// on a clock the test moves.
func pokerRig(t *testing.T, installed bool) (*rig, *testClock) {
	t.Helper()
	clock := &testClock{now: testStart}
	f := testFixture()
	site := &f.Jira[0]
	site.Users = append(site.Users, JiraUser{AccountID: "acc-user2", DisplayName: "User 2", Email: "user2@example.com"})
	site.Members = []JiraMember{{Account: "acc-user2", Token: memberToken}}
	if installed {
		site.Poker = &JiraPoker{}
	}
	g := &rig{e: New(Options{Now: clock.Now}), h: map[Origin]http.Handler{}}
	t.Cleanup(g.e.Close)
	if err := g.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	h, err := g.e.Handler(jiraOrigin)
	if err != nil {
		t.Fatal(err)
	}
	g.h[jiraOrigin] = h
	return g, clock
}

// asMember is header signing a request in as User 2.
func asMember() map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("user2@example.com:"+memberToken))}
}

// pokerClient drives the measured calls as one account.
type pokerClient struct {
	t      *testing.T
	g      *rig
	header map[string]string
}

func (p pokerClient) json(method, path string, body any, out any) int {
	p.t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			p.t.Fatal(err)
		}
		raw = string(b)
	}
	rec := p.g.do(call{origin: jiraOrigin, method: method, path: path, body: raw, header: p.header})
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			p.t.Fatalf("%s %s answered %s: %v", method, path, rec.Body, err)
		}
	}
	return rec.Code
}

// panel is the poker panel the listing names, failing when it names none.
func (p pokerClient) panel() map[string]any {
	p.t.Helper()
	var ans struct {
		Data struct {
			ExtensionContexts []struct {
				ExtensionsByType []map[string]any `json:"extensionsByType"`
			} `json:"extensionContexts"`
		} `json:"data"`
	}
	if code := p.json("POST", "/gateway/api/graphql", map[string]any{"operationName": "Ext", "query": pokerListDocument,
		"variables": map[string]any{"contextIds": []string{"ari:cloud:jira::site/" + defaultCloudID}}}, &ans); code != 200 {
		p.t.Fatalf("Ext answered %d", code)
	}
	for _, e := range ans.Data.ExtensionContexts[0].ExtensionsByType {
		if e["key"] == pokerPanelKey {
			return e
		}
	}
	return nil
}

var webOne = map[string]any{"type": "jira:issuePanel",
	"issue":   map[string]string{"key": "WEB-1", "id": "10101", "type": "Story", "typeId": "10001"},
	"project": map[string]string{"id": "10000", "key": "WEB", "type": "software"}}

// token signs a context token for WEB-1 and returns it with its claims.
func (p pokerClient) token() (string, map[string]any) {
	p.t.Helper()
	panel := p.panel()
	var ans struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expiresAt"`
		ExpiresIn int64  `json:"expiresIn"`
	}
	if code := p.json("POST", "/rest/internal/2/forge/context/token",
		map[string]any{"extension": panel, "extensionData": webOne, "localId": panel["id"], "useWorkspaceAri": true}, &ans); code != 200 {
		p.t.Fatalf("token answered %d", code)
	}
	parts := strings.Split(ans.Token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		p.t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		p.t.Fatal(err)
	}
	if ans.ExpiresIn != 900_000 || ans.ExpiresAt != testStart.Add(900*time.Second).UnixMilli() {
		p.t.Fatalf("expiresIn %d, expiresAt %d", ans.ExpiresIn, ans.ExpiresAt)
	}
	return ans.Token, claims
}

// invokeAnswer is the parts of an invoke's answer a test reads.
type invokeAnswer struct {
	Data struct {
		InvokeExtension struct {
			Success bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
			Response *struct {
				Body struct {
					Status int    `json:"status"`
					Body   string `json:"body"`
				} `json:"body"`
			} `json:"response"`
		} `json:"invokeExtension"`
	} `json:"data"`
}

func (p pokerClient) invoke(token string, contextIDs any, method, path string, body any) invokeAnswer {
	p.t.Helper()
	panel := p.panel()
	payload := map[string]any{
		"call":    map[string]any{"functionKey": "requestConnectBackend", "payload": map[string]any{"path": pokerRoutesRoot + path, "method": method, "headers": map[string]string{}, "body": body}},
		"context": map[string]any{"cloudId": defaultCloudID, "localId": panel["id"], "moduleKey": pokerPanelKey, "extension": webOne},
	}
	if token != "" {
		payload["contextToken"] = token
	}
	var ans invokeAnswer
	if code := p.json("POST", "/gateway/api/graphql", map[string]any{"operationName": "Invoke", "query": pokerInvokeDocument,
		"variables": map[string]any{"input": map[string]any{"contextIds": contextIDs, "extensionId": panel["id"], "entryPoint": "resolver", "payload": payload}}}, &ans); code != 200 {
		p.t.Fatalf("Invoke answered %d", code)
	}
	return ans
}

// round reads WEB-1's round as the poker app answers it.
func (p pokerClient) round(token string, ids any) (revealed bool, votes map[string]float64) {
	p.t.Helper()
	ans := p.invoke(token, ids, "GET", "planningPoker/10101", nil)
	if !ans.Data.InvokeExtension.Success || ans.Data.InvokeExtension.Response.Body.Status != 200 {
		p.t.Fatalf("read answered %+v", ans)
	}
	var r struct {
		IssueID    string `json:"issueId"`
		IsRevealed bool   `json:"isRevealed"`
		Votes      []struct {
			UserID string  `json:"userId"`
			Vote   float64 `json:"vote"`
		} `json:"votes"`
	}
	if err := json.Unmarshal([]byte(ans.Data.InvokeExtension.Response.Body.Body), &r); err != nil || r.IssueID != "10101" {
		p.t.Fatalf("read body %q: %v", ans.Data.InvokeExtension.Response.Body.Body, err)
	}
	votes = map[string]float64{}
	for _, v := range r.Votes {
		votes[v.UserID] = v.Vote
	}
	return r.IsRevealed, votes
}

// TestPokerCallsAsMeasured drives the measured calls as two people:
// cloud ID, listing, token, then read, vote, reveal and clear through the
// invoke, each account's vote its own.
func TestPokerCallsAsMeasured(t *testing.T) {
	g, _ := pokerRig(t, true)
	me, other := pokerClient{t: t, g: g}, pokerClient{t: t, g: g, header: asMember()}

	var tenant struct {
		CloudID string `json:"cloudId"`
	}
	if code := me.json("GET", "/_edge/tenant_info", nil, &tenant); code != 200 || tenant.CloudID != defaultCloudID {
		t.Fatalf("tenant_info answered %d %+v", code, tenant)
	}
	token, claims := me.token()
	ids := claims["contextIds"]
	if claims["accountId"] != "acc-user1" || len(ids.([]any)) != 1 || !strings.HasPrefix(ids.([]any)[0].(string), "ari:cloud:jira:"+defaultCloudID+":workspace/") {
		t.Fatalf("claims %v", claims)
	}
	otherToken, otherClaims := other.token()
	if otherClaims["accountId"] != "acc-user2" {
		t.Fatalf("the member's token names %v", otherClaims["accountId"])
	}

	if revealed, votes := me.round(token, ids); revealed || len(votes) != 0 {
		t.Fatalf("a new round: revealed %v, votes %v", revealed, votes)
	}
	for _, v := range []struct {
		p     pokerClient
		token string
		vote  float64
	}{{me, token, 5}, {other, otherToken, 8}, {me, token, 3}} {
		ans := v.p.invoke(v.token, ids, "PUT", "planningPoker/vote", map[string]any{"issueId": "10101", "vote": v.vote})
		if !ans.Data.InvokeExtension.Success || ans.Data.InvokeExtension.Response.Body.Status != 200 {
			t.Fatalf("vote answered %+v", ans)
		}
	}
	if revealed, votes := me.round(token, ids); revealed || votes["acc-user1"] != 3 || votes["acc-user2"] != 8 || len(votes) != 2 {
		t.Fatalf("after the votes: revealed %v, votes %v", revealed, votes)
	}
	if ans := other.invoke(otherToken, ids, "PUT", "planningPoker/reveal/10101", nil); ans.Data.InvokeExtension.Response.Body.Status != 200 {
		t.Fatalf("reveal answered %+v", ans)
	}
	if revealed, _ := me.round(token, ids); !revealed {
		t.Fatal("the round is not revealed")
	}
	ans := me.invoke(token, ids, "DELETE", "planningPoker/votes/10101", nil)
	if ans.Data.InvokeExtension.Response.Body.Status != 204 || ans.Data.InvokeExtension.Response.Body.Body != "" {
		t.Fatalf("clear answered %+v", ans)
	}
	if revealed, votes := me.round(token, ids); revealed || len(votes) != 0 {
		t.Fatalf("after clear: revealed %v, votes %v", revealed, votes)
	}
	if snap := g.e.Snapshot().Jira[0]; snap.Members[0].Token != "" {
		t.Fatal("the snapshot holds a member's token")
	}
}

// TestPokerGatewayRefusals: no token, contextIds other than the token's
// claim, an expired token and a vote out of range each fail and change
// nothing; on a site without the add-on the listing names no poker panel.
func TestPokerGatewayRefusals(t *testing.T) {
	g, clock := pokerRig(t, true)
	me := pokerClient{t: t, g: g}
	token, claims := me.token()
	ids := claims["contextIds"]
	for name, ans := range map[string]invokeAnswer{
		pokerMissingToken:  me.invoke("", ids, "GET", "planningPoker/10101", nil),
		pokerClaimMismatch: me.invoke(token, []string{"ari:cloud:jira::site/" + defaultCloudID}, "GET", "planningPoker/10101", nil),
	} {
		inv := ans.Data.InvokeExtension
		if inv.Success || inv.Response != nil || len(inv.Errors) != 1 || inv.Errors[0].Message != name {
			t.Errorf("%s: answered %+v", name, inv)
		}
	}
	if ans := me.invoke(token, ids, "PUT", "planningPoker/vote", map[string]any{"issueId": "10101", "vote": 1001}); ans.Data.InvokeExtension.Response.Body.Status != 400 {
		t.Errorf("a vote of 1001 answered %+v", ans)
	}
	if r := g.e.Snapshot().Jira[0].Poker; len(r.Rounds) != 0 {
		t.Errorf("a refused vote left %+v", r.Rounds)
	}
	clock.set(testStart.Add(900 * time.Second))
	if ans := me.invoke(token, ids, "GET", "planningPoker/10101", nil); ans.Data.InvokeExtension.Success {
		t.Error("an expired token was taken")
	}

	bare, _ := pokerRig(t, false)
	if panel := (pokerClient{t: t, g: bare}).panel(); panel != nil {
		t.Fatalf("a site without the add-on lists %v", panel)
	}
}

// TestPokerTokensAreNotLogged: no logged request repeats a context token
// the site issued or a member's token.
func TestPokerTokensAreNotLogged(t *testing.T) {
	g, _ := pokerRig(t, true)
	me, other := pokerClient{t: t, g: g}, pokerClient{t: t, g: g, header: asMember()}
	token, claims := me.token()
	otherToken, _ := other.token()
	me.round(token, claims["contextIds"])
	other.round(otherToken, claims["contextIds"])
	raw, err := json.Marshal(g.e.Requests(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, otherToken, memberToken} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the request log repeats a token: %s", raw)
		}
	}
	if !strings.Contains(string(raw), `"contextToken":"\u003credacted\u003e"`) {
		t.Fatalf("the invoke's token was not replaced: %s", raw)
	}
}

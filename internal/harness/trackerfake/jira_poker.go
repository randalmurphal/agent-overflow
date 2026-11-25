package trackerfake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Planning poker: the Easy Agile TeamRhythm add-on's calls as a site
// answers them (docs/references/tracker-api-measurements.md, "Planning
// poker add-on"; measured 2026-11-15, and read-only on 2026-11-24): the
// site's cloud ID, the extension listing and the invoke on the gateway,
// and the context token, which names one issue and one account. The
// votes live in JiraSite.Poker. A context token is a credential: the
// engine keeps the ones it issued apart from the state (jiraSite.issued)
// and scrubs them from the log (Engine.secrets).

// The defaults a site's fixture leaves out: a made-up cloud ID, workspace
// and the add-on's identity, in the measured forms.
const (
	defaultCloudID       = "11111111-1111-4111-8111-111111111111"
	defaultWorkspaceID   = "22222222-2222-4222-8222-222222222222"
	pokerAppID           = "33333333-3333-4333-8333-333333333333"
	pokerEnvironmentID   = "44444444-4444-4444-8444-444444444444"
	pokerInstallationID  = "55555555-5555-4555-8555-555555555555"
	pokerAppVersion      = "1.0.0"
	pokerPanelKey        = "planning-poker-forge-panel"
	pokerPanelType       = "jira:issuePanel"
	pokerRoutesRoot      = "/forge/rest/eausm/latest/"
	pokerTokenLife       = 900 * time.Second
	pokerMissingToken    = "Missing Forge Context Token"
	pokerClaimMismatch   = "FCT_VALIDATION_CLAIM_MISMATCH"
	otherPanelKey        = "other-issue-panel"
	pokerListDocument    = `query Ext($contextIds:[ID!]!){ extensionContexts(contextIds:$contextIds){ id extensionsByType(type:"jira:issuePanel"){ id key type appId environmentId installationId appVersion } } }`
	pokerInvokeDocument  = `mutation Invoke($input: InvokeExtensionInput!){ invokeExtension(input:$input){ success errors { message extensions { errorType statusCode } } response { body } } }`
	gatewayListOperation = "Ext"
	gatewayInvoke        = "Invoke"
)

// JiraPoker is a site's planning poker add-on, installed and enabled:
// each issue's round of votes.
type JiraPoker struct {
	Rounds []JiraPokerRound `json:"rounds,omitempty"`
}

// JiraPokerRound is one issue's votes, by the issue's ID.
type JiraPokerRound struct {
	Issue    string          `json:"issue"`
	Revealed bool            `json:"revealed,omitempty"`
	Votes    []JiraPokerVote `json:"votes,omitempty"`
}

// JiraPokerVote is one account's vote.
type JiraPokerVote struct {
	ID      string    `json:"id"`
	Account string    `json:"account"`
	Vote    float64   `json:"vote"`
	Created time.Time `json:"created"`
}

// JiraMember is a person who signs in to the site besides its Account, as
// a teammate does: requests with their email and token act as them.
type JiraMember struct {
	Account string `json:"account"`
	Token   string `json:"token,omitempty"`
}

// issuedToken is a context token the site issued: the account and issue
// it names, its contextIds claim and when it expires by the engine clock.
type issuedToken struct {
	account, issue string
	data           pokerExtensionData
	contextIDs     []string
	expires        time.Time
}

// pokerExtension is the add-on's panel as the listing answers it.
type pokerExtension struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	Type           string `json:"type"`
	AppID          string `json:"appId"`
	EnvironmentID  string `json:"environmentId"`
	InstallationID string `json:"installationId"`
	AppVersion     string `json:"appVersion"`
}

type pokerIssue struct {
	Key    string `json:"key"`
	ID     string `json:"id"`
	Type   string `json:"type"`
	TypeID string `json:"typeId"`
}

type pokerProject struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Type string `json:"type"`
}

type pokerExtensionData struct {
	Type    string       `json:"type"`
	Issue   pokerIssue   `json:"issue"`
	Project pokerProject `json:"project"`
}

// pokerPanel is the add-on's panel.
func pokerPanel() pokerExtension {
	return pokerExtension{
		ID:  "ari:cloud:ecosystem::extension/" + pokerAppID + "/" + pokerEnvironmentID + "/static/" + pokerPanelKey,
		Key: pokerPanelKey, Type: pokerPanelType, AppID: pokerAppID, EnvironmentID: pokerEnvironmentID,
		InstallationID: "ari:cloud:ecosystem::installation/" + pokerInstallationID, AppVersion: pokerAppVersion,
	}
}

// otherPanel is another app's issue panel: the listing named five on the
// measured site, so a client must pick the poker panel by its key.
func otherPanel() pokerExtension {
	return pokerExtension{
		ID:  "ari:cloud:ecosystem::extension/" + pokerEnvironmentID + "/" + pokerAppID + "/static/" + otherPanelKey,
		Key: otherPanelKey, Type: pokerPanelType, AppID: pokerEnvironmentID, EnvironmentID: pokerAppID,
		InstallationID: "ari:cloud:ecosystem::installation/" + pokerAppID, AppVersion: "2.0.1",
	}
}

func (s *jiraSite) workspaceARI() string {
	return "ari:cloud:jira:" + s.CloudID + ":workspace/" + defaultWorkspaceID
}

// seedPoker completes the cloud ID and the members, whose tokens leave the
// state as the site's does, and checks the poker rounds.
func (s *jiraSite) seedPoker() error {
	if s.CloudID == "" {
		s.CloudID = defaultCloudID
	}
	s.members = map[string]string{}
	s.Members = slices.Clone(s.Members)
	for i := range s.Members {
		m := &s.Members[i]
		u := s.user(m.Account)
		switch {
		case u == nil || u.Email == "" || m.Account == s.Account:
			return fmt.Errorf("member %q is no other user with an email", m.Account)
		case checkToken("member "+m.Account+"'s token", m.Token) != nil:
			return checkToken("member "+m.Account+"'s token", m.Token)
		}
		s.members[m.Account] = m.Token
		m.Token = ""
	}
	if s.Poker == nil {
		return nil
	}
	s.Poker = &JiraPoker{Rounds: slices.Clone(s.Poker.Rounds)}
	for _, r := range s.Poker.Rounds {
		if s.issue(r.Issue) == nil {
			return fmt.Errorf("poker round on %q, which is no issue", r.Issue)
		}
		for _, v := range r.Votes {
			if s.user(v.Account) == nil || v.ID == "" {
				return fmt.Errorf("poker vote %q on %s by %q, who is no user", v.ID, r.Issue, v.Account)
			}
		}
	}
	return nil
}

// tenantInfo is the site's cloud ID (measured: GET _edge/tenant_info
// answers {"cloudId": <uuid>}).
func tenantInfo(c *jiraCall) answer {
	if err := c.noBody(); err != nil {
		return unhandledf("%v", err)
	}
	return jsonAnswer(http.StatusOK, map[string]string{"cloudId": c.s.CloudID})
}

// gatewayRequest is a request to the GraphQL gateway.
type gatewayRequest struct {
	OperationName string          `json:"operationName"`
	Query         string          `json:"query"`
	Variables     json.RawMessage `json:"variables"`
}

// gateway answers the two measured documents and nothing else.
func gateway(c *jiraCall) answer {
	var req gatewayRequest
	if err := c.decode(&req, false); err != nil {
		return unhandledf("%v", err)
	}
	switch {
	case req.OperationName == gatewayListOperation && req.Query == pokerListDocument:
		return c.listExtensions(req.Variables)
	case req.OperationName == gatewayInvoke && req.Query == pokerInvokeDocument:
		return c.invoke(req.Variables)
	}
	return unhandledf("gateway operation %q with a document other than the measured ones", req.OperationName)
}

// listExtensions lists the site's issue panels (measured: one context
// whose extensionsByType lists every issue panel, all members strings;
// the poker panel only while the add-on is installed).
func (c *jiraCall) listExtensions(raw json.RawMessage) answer {
	var vars struct {
		ContextIDs []string `json:"contextIds"`
	}
	if err := decodeStrict(raw, &vars, false); err != nil {
		return unhandledf("Ext variables: %v", err)
	}
	site := "ari:cloud:jira::site/" + c.s.CloudID
	if !slices.Equal(vars.ContextIDs, []string{site}) {
		return unhandledf("Ext for contexts %q; the fake answers its own site's, %s", vars.ContextIDs, site)
	}
	panels := []pokerExtension{otherPanel()}
	if c.s.Poker != nil {
		panels = append(panels, pokerPanel())
	}
	return jsonAnswer(http.StatusOK, map[string]any{"data": map[string]any{
		"extensionContexts": []any{map[string]any{"id": site, "extensionsByType": panels}},
	}})
}

// contextToken signs a context token for one issue of the caller's
// (measured: the answer's token is a JWT whose payload names the account,
// the issue and a top-level contextIds of one workspace ARI, expiresIn is
// milliseconds and exp - iat 900 seconds).
func contextToken(c *jiraCall) answer {
	var req struct {
		Extension       pokerExtension     `json:"extension"`
		ExtensionData   pokerExtensionData `json:"extensionData"`
		LocalID         string             `json:"localId"`
		UseWorkspaceAri bool               `json:"useWorkspaceAri"`
	}
	if err := c.decode(&req, false); err != nil {
		return unhandledf("%v", err)
	}
	panel := pokerPanel()
	switch {
	case c.s.Poker == nil:
		return unhandledf("a context token on a site without the poker add-on")
	case req.Extension != panel || req.LocalID != panel.ID || !req.UseWorkspaceAri || req.ExtensionData.Type != pokerPanelType:
		return unhandledf("a context token for another extension than the listing named, or without useWorkspaceAri")
	}
	if why := c.s.checkExtensionData(req.ExtensionData); why != "" {
		return unhandledf("a context token with %s", why)
	}
	now := c.e.now()
	for k, t := range c.s.issued {
		if !now.Before(t.expires) {
			delete(c.s.issued, k)
		}
	}
	tok := issuedToken{account: c.viewer, issue: req.ExtensionData.Issue.ID, data: req.ExtensionData,
		contextIDs: []string{c.s.workspaceARI()}, expires: now.Add(pokerTokenLife)}
	token := c.s.signToken(c.e.nextID(), tok, now)
	c.s.issued[token] = tok
	return jsonAnswer(http.StatusOK, map[string]any{
		"token": token, "expiresAt": tok.expires.UnixMilli(), "expiresIn": pokerTokenLife.Milliseconds(), "extensionId": panel.ID,
	})
}

// checkExtensionData names what in data is not the real issue and project
// (the measurement sent real values; made-up ones are not modeled).
func (s *jiraSite) checkExtensionData(data pokerExtensionData) string {
	is := s.issue(data.Issue.ID)
	if is == nil {
		return "issue " + data.Issue.ID + ", which is no issue"
	}
	p := s.project(is.Project)
	t := p.issueType(is.Type)
	if t == nil {
		return "issue " + is.Key + ", whose type its project lacks"
	}
	want := pokerExtensionData{Type: pokerPanelType,
		Issue:   pokerIssue{Key: is.Key, ID: is.ID, Type: t.Name, TypeID: t.ID},
		Project: pokerProject{ID: p.ID, Key: p.Key, Type: p.TypeKey}}
	if data != want {
		return fmt.Sprintf("extension data %+v, not the issue's %+v", data, want)
	}
	return ""
}

// signToken is a JWT carrying tok's claims in the measured names, with a
// signature no one checks; seq makes each token unique.
func (s *jiraSite) signToken(seq int64, tok issuedToken, now time.Time) string {
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(fmt.Sprintf("trackerfake: encode a token: %v", err))
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	panel := pokerPanel()
	claims := map[string]any{
		"accountId": tok.account, "appId": pokerAppID, "appVersion": pokerAppVersion, "aud": "ari:cloud:ecosystem::app/" + pokerAppID,
		"context": map[string]any{
			"accountId": tok.account, "cloudId": s.CloudID, "environmentId": pokerEnvironmentID, "environmentType": "PRODUCTION",
			"extension": map[string]any{"type": pokerPanelType, "issue": tok.data.Issue, "project": tok.data.Project},
			"localId":   panel.ID, "moduleKey": pokerPanelKey, "siteUrl": "https://" + s.Site,
		},
		"contextIds": tok.contextIDs, "exp": tok.expires.Unix(), "extensionId": panel.ID, "extensionType": pokerPanelType,
		"iat": now.Unix(), "installationId": panel.InstallationID, "iss": "forge/context-token", "jti": strconv.FormatInt(seq, 10), "nbf": now.Unix(),
	}
	return enc(map[string]string{"alg": "RS256", "kid": "trackerfake"}) + "." + enc(claims) + "." +
		base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "trackerfake-signature-%d", seq))
}

// invokeInput is invokeExtension's input as the measured call sends it.
type invokeInput struct {
	ContextIDs  []string `json:"contextIds"`
	ExtensionID string   `json:"extensionId"`
	EntryPoint  string   `json:"entryPoint"`
	Payload     struct {
		Call struct {
			FunctionKey string `json:"functionKey"`
			Payload     struct {
				Path    string            `json:"path"`
				Method  string            `json:"method"`
				Headers map[string]string `json:"headers"`
				Body    json.RawMessage   `json:"body"`
			} `json:"payload"`
		} `json:"call"`
		Context struct {
			CloudID   string             `json:"cloudId"`
			LocalID   string             `json:"localId"`
			ModuleKey string             `json:"moduleKey"`
			Extension pokerExtensionData `json:"extension"`
		} `json:"context"`
		ContextToken *string `json:"contextToken"`
	} `json:"payload"`
}

// invoke runs one poker app request through invokeExtension: the gateway
// checks the token, then the poker app answers inside response.body
// (measured: {status, statusText, headers, body}, body a JSON document in
// a string).
func (c *jiraCall) invoke(raw json.RawMessage) answer {
	var vars struct {
		Input invokeInput `json:"input"`
	}
	if err := decodeStrict(raw, &vars, false); err != nil {
		return unhandledf("Invoke variables: %v", err)
	}
	in := vars.Input
	panel := pokerPanel()
	call := in.Payload.Call
	switch {
	case in.ExtensionID != panel.ID || in.EntryPoint != "resolver" || call.FunctionKey != "requestConnectBackend":
		return unhandledf("an invoke of another extension, entry point or function than the poker panel's requestConnectBackend")
	case in.Payload.Context.CloudID != c.s.CloudID || in.Payload.Context.LocalID != panel.ID || in.Payload.Context.ModuleKey != pokerPanelKey:
		return unhandledf("an invoke whose context names another site or module")
	case len(call.Payload.Headers) > 0:
		return unhandledf("backend request headers %v", slices.Sorted(maps.Keys(call.Payload.Headers)))
	}
	// Measured: without contextToken the call fails with "Missing Forge
	// Context Token", and with contextIds other than the token's claim
	// with FCT_VALIDATION_CLAIM_MISMATCH. Where those errors sit, their
	// errorType and statusCode, and the answer to an expired or unknown
	// token are unverified: a failed invoke with a 4xx statusCode.
	if in.Payload.ContextToken == nil {
		return invokeFailed(pokerMissingToken, 401)
	}
	tok, ok := c.s.issued[*in.Payload.ContextToken]
	switch {
	case !ok || !c.e.now().Before(tok.expires):
		return invokeFailed("The context token is invalid or expired", 401)
	case !slices.Equal(in.ContextIDs, tok.contextIDs):
		return invokeFailed(pokerClaimMismatch, 400)
	case in.Payload.Context.Extension != tok.data:
		return invokeFailed("The context does not match the context token", 400)
	}
	return c.pokerBackend(tok, call.Payload.Method, call.Payload.Path, call.Payload.Body)
}

// invokeFailed is a gateway refusal of an invoke (unverified, see invoke).
func invokeFailed(message string, status int) answer {
	return jsonAnswer(http.StatusOK, map[string]any{"data": map[string]any{"invokeExtension": map[string]any{
		"success": false, "response": nil,
		"errors": []any{map[string]any{"message": message, "extensions": map[string]any{"errorType": "FORGE_CONTEXT_TOKEN", "statusCode": status}}},
	}}})
}

// backendAnswer is the poker app's answer wrapped as the invoke returns
// it.
func backendAnswer(status int, body any) answer {
	text := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			panic(fmt.Sprintf("trackerfake: encode a poker answer: %v", err))
		}
		text = string(b)
	}
	headers := map[string]string{}
	if body != nil {
		headers["content-type"] = "application/json; charset=utf-8"
	}
	return jsonAnswer(http.StatusOK, map[string]any{"data": map[string]any{"invokeExtension": map[string]any{
		"success": true, "errors": nil,
		"response": map[string]any{"body": map[string]any{"status": status, "statusText": http.StatusText(status), "headers": headers, "body": text}},
	}}})
}

// pokerBackend answers the poker app's routes: paths, methods, bodies and
// answers as recorded 2026-11-15, the read measured again 2026-11-24. The
// 2026-11-24 measurement made no write, so a write body's encoding (the
// JSON value, not a string holding it) is unverified.
func (c *jiraCall) pokerBackend(tok issuedToken, method, path string, body json.RawMessage) answer {
	route, ok := strings.CutPrefix(path, pokerRoutesRoot)
	if !ok {
		return unhandledf("poker app path %q outside %s", path, pokerRoutesRoot)
	}
	hasBody := len(bytes.TrimSpace(body)) > 0 && string(bytes.TrimSpace(body)) != "null"
	switch {
	case method == http.MethodGet && strings.HasPrefix(route, "planningPoker/") && !hasBody:
		return c.pokerRead(tok, strings.TrimPrefix(route, "planningPoker/"), false)
	case method == http.MethodPut && route == "planningPoker/vote":
		return c.pokerVote(tok, body)
	case method == http.MethodPut && strings.HasPrefix(route, "planningPoker/reveal/") && !hasBody:
		return c.pokerRead(tok, strings.TrimPrefix(route, "planningPoker/reveal/"), true)
	case method == http.MethodDelete && strings.HasPrefix(route, "planningPoker/votes/") && !hasBody:
		return c.pokerClear(tok, strings.TrimPrefix(route, "planningPoker/votes/"))
	}
	return unhandledf("poker app request %s %s", method, path)
}

// round is issue's round, made when make is set and there is none.
func (p *JiraPoker) round(issue string, make bool) *JiraPokerRound {
	for i := range p.Rounds {
		if p.Rounds[i].Issue == issue {
			return &p.Rounds[i]
		}
	}
	if !make {
		return nil
	}
	p.Rounds = append(p.Rounds, JiraPokerRound{Issue: issue})
	return &p.Rounds[len(p.Rounds)-1]
}

// pokerTarget is the issue a poker request names, refused (unverified:
// a 404) unless it is the token's issue on this site.
func (c *jiraCall) pokerTarget(tok issuedToken, issue string) (string, *answer) {
	if issue != tok.issue || c.s.issue(issue) == nil {
		a := backendAnswer(http.StatusNotFound, map[string]string{"message": "Not Found"})
		return "", &a
	}
	return issue, nil
}

func voteJSON(issue string, v JiraPokerVote) map[string]any {
	return map[string]any{"id": v.ID, "issueId": issue, "userId": v.Account, "vote": v.Vote, "createdAt": v.Created.UTC().Format(time.RFC3339Nano)}
}

// pokerRead is the read shape (measured), revealing first when reveal is
// set.
func (c *jiraCall) pokerRead(tok issuedToken, issue string, reveal bool) answer {
	issue, refused := c.pokerTarget(tok, issue)
	if refused != nil {
		return *refused
	}
	r := c.s.Poker.round(issue, reveal)
	votes := []any{}
	revealed := false
	if r != nil {
		if reveal {
			r.Revealed = true
		}
		revealed = r.Revealed
		for _, v := range r.Votes {
			votes = append(votes, voteJSON(issue, v))
		}
	}
	return backendAnswer(http.StatusOK, map[string]any{"issueId": issue, "isRevealed": revealed, "votes": votes})
}

// pokerVote casts or changes the token's account's vote.
func (c *jiraCall) pokerVote(tok issuedToken, body json.RawMessage) answer {
	var in struct {
		IssueID string   `json:"issueId"`
		Vote    *float64 `json:"vote"`
	}
	if err := decodeStrict(body, &in, false); err != nil || in.Vote == nil {
		return unhandledf("a vote body other than {issueId, vote}: %v", err)
	}
	issue, refused := c.pokerTarget(tok, in.IssueID)
	if refused != nil {
		return *refused
	}
	// Documented: a vote is from 0 to 1000. The refusal's status and body
	// are unverified.
	if *in.Vote < 0 || *in.Vote > 1000 {
		return backendAnswer(http.StatusBadRequest, map[string]string{"message": "A vote is from 0 to 1000"})
	}
	r := c.s.Poker.round(issue, true)
	now := c.e.now()
	i := slices.IndexFunc(r.Votes, func(v JiraPokerVote) bool { return v.Account == tok.account })
	if i < 0 {
		r.Votes = append(r.Votes, JiraPokerVote{ID: strconv.FormatInt(c.e.nextID(), 10), Account: tok.account})
		i = len(r.Votes) - 1
	}
	r.Votes[i].Vote, r.Votes[i].Created = *in.Vote, now
	return backendAnswer(http.StatusOK, voteJSON(issue, r.Votes[i]))
}

// pokerClear removes every vote on the issue (recorded: 204). That the
// next round starts hidden is unverified.
func (c *jiraCall) pokerClear(tok issuedToken, issue string) answer {
	issue, refused := c.pokerTarget(tok, issue)
	if refused != nil {
		return *refused
	}
	if r := c.s.Poker.round(issue, false); r != nil {
		r.Votes, r.Revealed = nil, false
	}
	return backendAnswer(http.StatusNoContent, nil)
}

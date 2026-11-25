package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// Planning poker is the Easy Agile TeamRhythm add-on's, held on the
// vendor's servers and reached through two undocumented Atlassian
// endpoints on the site, with the site's own credential
// (docs/references/tracker-api-measurements.md, "Planning poker add-on",
// measured 2026-11-15 and 2026-11-24):
//
//  1. The site's cloud ID (GET _edge/tenant_info), which never changes and
//     is kept for the client's life.
//  2. The add-on's issue panel among the site's extensions (pokerListDoc),
//     kept siteAge, its absence included: one listing a connection per 30
//     minutes answers whether the site has the add-on.
//  3. A context token for one issue (pokerTokenPath), from the issue's own
//     key, type and project: the token names the issue, so it is per
//     issue. It is kept in memory only, at most pokerTokenCacheSize, used
//     until pokerTokenMargin before its expiresIn (milliseconds from its
//     answer, measured on this computer's monotonic clock, so a wall clock
//     step does not matter), and dropped when the gateway refuses a call
//     made with it.
//  4. invokeExtension (pokerInvokeDoc), carrying the vendor backend's
//     request, whose status and body come back inside the answer.
//
// The token is a *secret.Bytes and appears in no error, log line or
// answer: gateway messages that reach an error have it replaced by
// secret.Redacted.

const (
	tenantInfoPath  = "_edge/tenant_info"
	gatewayPath     = "gateway/api/graphql"
	pokerTokenPath  = "rest/internal/2/forge/context/token"
	pokerPanelKey   = "planning-poker-forge-panel"
	pokerPanelType  = "jira:issuePanel"
	pokerRoutesRoot = "/forge/rest/eausm/latest/"
	// pokerTokenMargin is how long before its expiry a context token is
	// no longer used: a call that takes up to the transport's read bound
	// still carries a live token.
	pokerTokenMargin = 60 * time.Second
	// pokerTokenCacheSize bounds the tokens kept: one per issue whose
	// poker is followed or written, which a person has a few of open.
	pokerTokenCacheSize = 16
	// maxGatewayMessage bounds the gateway's message an error repeats.
	maxGatewayMessage = 200
)

// The measured documents, sent as they are (2026-11-24).
const (
	pokerListDoc   = `query Ext($contextIds:[ID!]!){ extensionContexts(contextIds:$contextIds){ id extensionsByType(type:"jira:issuePanel"){ id key type appId environmentId installationId appVersion } } }`
	pokerInvokeDoc = `mutation Invoke($input: InvokeExtensionInput!){ invokeExtension(input:$input){ success errors { message extensions { errorType statusCode } } response { body } } }`
)

// The steps of a poker call, as a person reads which one failed.
const (
	pokerStepCloud   = "the site's cloud ID"
	pokerStepListing = "the site's extension listing"
	pokerStepToken   = "the poker context token"
	pokerStepGateway = "Atlassian's extension gateway"
	pokerStepApp     = "the poker app"
)

// ErrPokerAbsent is a poker call on a site whose extension listing has no
// planning poker panel: the add-on is not installed or not enabled.
var ErrPokerAbsent = errors.New("jira: planning poker is not installed on this site")

// PokerError is a poker call that failed in a way only poker shows: Step
// (which call) answered with something other than what was measured, or
// refused. Refused reports a call the gateway or the poker app answered
// with a 4xx: a write that was not applied. Any other PokerError of a
// write leaves its outcome unknown, unless Err, the transport's error it
// wraps, proves otherwise (trackerapi.WriteNotApplied). Detail holds no
// credential and no token, and a PokerError of the invoke wraps no part
// of the invoke's answer (sealInvoke).
type PokerError struct {
	Step    string
	Detail  string
	Refused bool
	Err     error
}

func (e *PokerError) Error() string { return "jira: planning poker: " + e.Step + " " + e.Detail }
func (e *PokerError) Unwrap() error { return e.Err }

// unrecognized is the PokerError of an answer of another shape than the
// measured one.
func unrecognized(step string, err error) *PokerError {
	return &PokerError{Step: step, Detail: "answered in a form Agent Overflow does not recognize", Err: err}
}

// pokerCallErr is err, the failure of step's request, as the client
// returns it: an answer that did not decode, or a status other than a
// refused credential or a rate limit, is the endpoint's and so poker's
// (PokerError); a transport failure, a refused credential and a rate
// limit are the connection's, as every call's.
func pokerCallErr(step string, err error) error {
	if _, ok := errors.AsType[*AnswerError](err); ok {
		return unrecognized(step, err)
	}
	if se, ok := outcomeOf(err).(*trackerapi.StatusError); ok {
		return &PokerError{Step: step, Detail: fmt.Sprintf("answered HTTP %d", se.Status), Refused: clientStatus(se.Status), Err: err}
	}
	return fmt.Errorf("jira: planning poker: %s: %w", step, err)
}

func clientStatus(status int) bool { return status >= 400 && status <= 499 }

// pokerExtension is the add-on's panel as the listing names it, sent back
// as it came.
type pokerExtension struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	Type           string `json:"type"`
	AppID          string `json:"appId"`
	EnvironmentID  string `json:"environmentId"`
	InstallationID string `json:"installationId"`
	AppVersion     string `json:"appVersion"`
}

func (e pokerExtension) complete() bool {
	return e.ID != "" && e.Key != "" && e.Type != "" && e.AppID != "" && e.EnvironmentID != "" && e.InstallationID != "" && e.AppVersion != ""
}

// pokerApp is the site's add-on: its cloud ID, and its panel, nil when
// the site has none.
type pokerApp struct {
	cloudID string
	ext     *pokerExtension
}

// pokerExtensionData is what a token is signed for: the issue panel on
// one issue of one project, with the issue's real values.
type pokerExtensionData struct {
	Type    string       `json:"type"`
	Issue   pokerIssue   `json:"issue"`
	Project pokerProject `json:"project"`
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

// pokerToken is one issue's context token: the token, its contextIds
// claim as it came (the invoke must carry it unchanged), what it was
// signed for, and until when it is used.
type pokerToken struct {
	token      *secret.Bytes
	contextIDs json.RawMessage
	data       pokerExtensionData
	until      time.Time
}

// gqlError is a GraphQL error as the gateway answers one.
type gqlError struct {
	Message    string `json:"message"`
	Extensions *struct {
		ErrorType  string `json:"errorType"`
		StatusCode int    `json:"statusCode"`
	} `json:"extensions"`
}

// gatewayMessage is errs' first message, cut to maxGatewayMessage bytes,
// with token replaced (redactToken).
func gatewayMessage(errs []gqlError, token *secret.Bytes) string {
	if len(errs) == 0 {
		return ""
	}
	msg := redactToken(errs[0].Message, token)
	if len(msg) > maxGatewayMessage {
		cut := maxGatewayMessage
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "..."
	}
	return msg
}

// PokerInstalled reports whether the site has the planning poker add-on:
// one listing a siteAge, the first after the cloud ID's one read, the
// two one walk.
func (c *Client) PokerInstalled(ctx context.Context) (bool, error) {
	if err := c.admit(ctx); err != nil {
		return false, err
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (bool, error) {
		app, err := c.pokerApp(ctx)
		return app.ext != nil, err
	})
}

// pokerCloud is the site's cloud ID.
func (c *Client) pokerCloud(ctx context.Context) (string, error) {
	if id, ok := c.cloudID.get(ctx, struct{}{}); ok {
		return id, nil
	}
	epoch := c.cloudID.epoch()
	var out struct {
		CloudID string `json:"cloudId"`
	}
	resp, err := c.call(ctx, trackerapi.Request{Path: tenantInfoPath}, &out)
	if err != nil {
		return "", pokerCallErr(pokerStepCloud, err)
	}
	if out.CloudID == "" {
		return "", unrecognized(pokerStepCloud, nil)
	}
	c.cloudID.put(epoch, resp.CredentialMark(), struct{}{}, out.CloudID)
	return out.CloudID, nil
}

// pokerApp is the site's add-on as the listing kept within siteAge
// names it.
func (c *Client) pokerApp(ctx context.Context) (pokerApp, error) {
	if app, ok := c.poker.get(ctx, struct{}{}); ok {
		return app, nil
	}
	cloud, err := c.pokerCloud(ctx)
	if err != nil {
		return pokerApp{}, err
	}
	epoch := c.poker.epoch()
	var out struct {
		Data *struct {
			ExtensionContexts []struct {
				ExtensionsByType []pokerExtension `json:"extensionsByType"`
			} `json:"extensionContexts"`
		} `json:"data"`
		Errors []gqlError `json:"errors"`
	}
	body := map[string]any{"operationName": "Ext", "query": pokerListDoc,
		"variables": map[string]any{"contextIds": []string{"ari:cloud:jira::site/" + cloud}}}
	resp, err := c.call(ctx, trackerapi.Request{Method: http.MethodPost, Path: gatewayPath, Body: body, ReadOnly: true}, &out)
	if err != nil {
		return pokerApp{}, pokerCallErr(pokerStepListing, err)
	}
	if len(out.Errors) > 0 {
		return pokerApp{}, &PokerError{Step: pokerStepListing, Detail: "refused the query: " + gatewayMessage(out.Errors, nil)}
	}
	if out.Data == nil || len(out.Data.ExtensionContexts) != 1 {
		return pokerApp{}, unrecognized(pokerStepListing, nil)
	}
	app := pokerApp{cloudID: cloud}
	for _, ext := range out.Data.ExtensionContexts[0].ExtensionsByType {
		if ext.Key != pokerPanelKey {
			continue
		}
		if !ext.complete() {
			return pokerApp{}, unrecognized(pokerStepListing, nil)
		}
		app.ext = &ext
		break
	}
	c.poker.put(epoch, resp.CredentialMark(), struct{}{}, app)
	return app, nil
}

// pokerToken is a context token for issue, the kept one while it is
// used, or a new one: the issue's key, type and project (one read), then
// the token.
func (c *Client) pokerToken(ctx context.Context, app pokerApp, issue string) (pokerToken, error) {
	if t, ok := c.pokerTokens.get(ctx, issue); ok && c.pokerTokens.clock().Before(t.until) {
		return t, nil
	}
	epoch := c.pokerTokens.epoch()
	is, err := c.GetIssue(ctx, issue, "project", "issuetype")
	if err != nil {
		return pokerToken{}, err
	}
	if is.Project == nil || is.Type == nil {
		return pokerToken{}, fmt.Errorf("jira: issue %s: %w", issue, &AnswerError{Message: "the answer names no project or issue type"})
	}
	data := pokerExtensionData{Type: pokerPanelType,
		Issue:   pokerIssue{Key: is.Key, ID: is.ID, Type: is.Type.Name, TypeID: is.Type.ID},
		Project: pokerProject{ID: is.Project.ID, Key: is.Project.Key, Type: is.Project.TypeKey}}
	body := map[string]any{"extension": app.ext, "extensionData": data, "localId": app.ext.ID, "useWorkspaceAri": true}
	var out struct {
		Token     string  `json:"token"`
		ExpiresIn float64 `json:"expiresIn"`
	}
	// Signing reads nothing and changes nothing: retried as a read.
	resp, err := c.call(ctx, trackerapi.Request{Method: http.MethodPost, Path: pokerTokenPath, Body: body, ReadOnly: true}, &out)
	if err != nil {
		return pokerToken{}, pokerCallErr(pokerStepToken, err)
	}
	ids, ok := tokenContextIDs(out.Token)
	if !ok || out.ExpiresIn <= 0 {
		return pokerToken{}, unrecognized(pokerStepToken, nil)
	}
	t := pokerToken{token: secret.New([]byte(out.Token)), contextIDs: ids, data: data,
		until: c.pokerTokens.clock().Add(time.Duration(out.ExpiresIn)*time.Millisecond - pokerTokenMargin)}
	c.pokerTokens.put(epoch, resp.CredentialMark(), issue, t)
	return t, nil
}

// tokenContextIDs is the contextIds claim of token, a JWT, as it came: a
// list of one or more strings at the top of its payload (measured).
func tokenContextIDs(token string) (json.RawMessage, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims struct {
		ContextIDs json.RawMessage `json:"contextIds"`
	}
	var ids []string
	if json.Unmarshal(payload, &claims) != nil || json.Unmarshal(claims.ContextIDs, &ids) != nil || len(ids) == 0 {
		return nil, false
	}
	for _, id := range ids {
		if id == "" {
			return nil, false
		}
	}
	return claims.ContextIDs, true
}

// pokerAnswer is the poker app's answer to one invoke: its status, status
// text and body.
type pokerAnswer struct {
	status     int
	statusText string
	body       string
}

// redactToken is text with token replaced by secret.Redacted: the
// gateway and the poker app may repeat the token they were sent.
func redactToken(text string, token *secret.Bytes) string {
	if t := token.Reveal(); len(t) > 0 {
		return strings.ReplaceAll(text, string(t), secret.Redacted)
	}
	return text
}

// sealInvoke is err, the invoke's failed request, as pokerInvoke returns
// it, holding nothing of the invoke's answer, which may repeat the token:
// a status the gateway answered is a PokerError that wraps no
// *trackerapi.StatusError (its Body, Message and Fields are the answer's),
// and a refused credential keeps its message with token replaced. Every
// other failure (a rate limit, the network, an ended call, a request not
// sent, an answer that did not decode) holds no text of the answer and is
// pokerCallErr's.
func sealInvoke(err error, token *secret.Bytes) error {
	if se, ok := outcomeOf(err).(*trackerapi.StatusError); ok {
		return &PokerError{Step: pokerStepGateway, Detail: fmt.Sprintf("answered HTTP %d", se.Status), Refused: clientStatus(se.Status)}
	}
	if ae, ok := err.(*trackerapi.AuthError); ok {
		sealed := *ae
		sealed.Message = redactToken(ae.Message, token)
		err = &sealed
	}
	return pokerCallErr(pokerStepGateway, err)
}

// pokerInvoke sends the poker app method and path (under
// pokerRoutesRoot) for issue, with body, read marking a call that changes
// nothing. A site without the add-on fails with ErrPokerAbsent before any
// request of the token or the invoke; every failure before the invoke is
// sent is a *trackerapi.NotSentError, so a write it stops is known not
// sent.
func (c *Client) pokerInvoke(ctx context.Context, issue, method, path string, body any, read bool) (pokerAnswer, error) {
	app, err := c.pokerApp(ctx)
	switch {
	case err != nil:
		return pokerAnswer{}, &trackerapi.NotSentError{Err: err}
	case app.ext == nil:
		return pokerAnswer{}, &trackerapi.NotSentError{Err: ErrPokerAbsent}
	}
	tok, err := c.pokerToken(ctx, app, issue)
	if err != nil {
		return pokerAnswer{}, &trackerapi.NotSentError{Err: err}
	}
	input := map[string]any{
		"contextIds":  tok.contextIDs,
		"extensionId": app.ext.ID,
		"entryPoint":  "resolver",
		"payload": map[string]any{
			"call": map[string]any{"functionKey": "requestConnectBackend", "payload": map[string]any{
				"path": pokerRoutesRoot + path, "method": method, "headers": map[string]string{},
				// The JSON value itself. Measured for no write (2026-11-24):
				// if the backend wants the body as a string, this changes.
				"body": body,
			}},
			"context":      map[string]any{"cloudId": app.cloudID, "localId": app.ext.ID, "moduleKey": pokerPanelKey, "extension": tok.data},
			"contextToken": string(tok.token.Reveal()),
		},
	}
	var out struct {
		Data *struct {
			InvokeExtension *struct {
				Success  bool       `json:"success"`
				Errors   []gqlError `json:"errors"`
				Response *struct {
					Body *struct {
						Status     int     `json:"status"`
						StatusText string  `json:"statusText"`
						Body       *string `json:"body"`
					} `json:"body"`
				} `json:"response"`
			} `json:"invokeExtension"`
		} `json:"data"`
		Errors []gqlError `json:"errors"`
	}
	req := trackerapi.Request{Method: http.MethodPost, Path: gatewayPath, ReadOnly: read,
		Body: map[string]any{"operationName": "Invoke", "query": pokerInvokeDoc, "variables": map[string]any{"input": input}}}
	if _, err := c.call(ctx, req, &out); err != nil {
		return pokerAnswer{}, sealInvoke(err, tok.token)
	}
	if out.Data == nil || out.Data.InvokeExtension == nil {
		c.pokerTokens.drop(issue)
		if len(out.Errors) > 0 {
			return pokerAnswer{}, &PokerError{Step: pokerStepGateway, Detail: "refused the call: " + gatewayMessage(out.Errors, tok.token)}
		}
		return pokerAnswer{}, unrecognized(pokerStepGateway, nil)
	}
	inv := out.Data.InvokeExtension
	if !inv.Success {
		c.pokerTokens.drop(issue)
		refused := len(inv.Errors) > 0 && inv.Errors[0].Extensions != nil && clientStatus(inv.Errors[0].Extensions.StatusCode)
		return pokerAnswer{}, &PokerError{Step: pokerStepGateway, Detail: "refused the call: " + gatewayMessage(inv.Errors, tok.token), Refused: refused}
	}
	if inv.Response == nil || inv.Response.Body == nil || inv.Response.Body.Body == nil || inv.Response.Body.Status == 0 {
		return pokerAnswer{}, unrecognized(pokerStepApp, nil)
	}
	a := pokerAnswer{status: inv.Response.Body.Status, statusText: redactToken(inv.Response.Body.StatusText, tok.token),
		body: redactToken(*inv.Response.Body.Body, tok.token)}
	switch {
	case a.status >= 200 && a.status <= 299:
		return a, nil
	case clientStatus(a.status):
		return pokerAnswer{}, &PokerError{Step: pokerStepApp, Detail: "refused it (HTTP " + a.statusLine() + ")", Refused: true}
	}
	return pokerAnswer{}, &PokerError{Step: pokerStepApp, Detail: "failed (HTTP " + a.statusLine() + ")"}
}

func (a pokerAnswer) statusLine() string {
	if a.statusText == "" {
		return fmt.Sprint(a.status)
	}
	return fmt.Sprintf("%d %s", a.status, a.statusText)
}

// PokerRound is an issue's votes as the poker app answers them: every
// vote's value, revealed or not. Hiding them is the caller's.
type PokerRound struct {
	Revealed bool
	Votes    []PokerVote
}

// PokerVote is one account's vote.
type PokerVote struct {
	Account string
	Vote    float64
}

// PokerVotes reads issue's votes; issue is a numeric issue ID. The
// listing, the issue, the token and the invoke are one walk: the token is
// the caller's, so the votes it marks as the caller's are one account's.
func (c *Client) PokerVotes(ctx context.Context, issue string) (PokerRound, error) {
	if err := c.admit(ctx); err != nil {
		return PokerRound{}, err
	}
	if err := checkID("issue", issue); err != nil {
		return PokerRound{}, err
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (PokerRound, error) { return c.pokerVotes(ctx, issue) })
}

func (c *Client) pokerVotes(ctx context.Context, issue string) (PokerRound, error) {
	a, err := c.pokerInvoke(ctx, issue, http.MethodGet, "planningPoker/"+issue, nil, true)
	if err != nil {
		return PokerRound{}, err
	}
	var round struct {
		IsRevealed *bool `json:"isRevealed"`
		Votes      *[]struct {
			UserID string   `json:"userId"`
			Vote   *float64 `json:"vote"`
		} `json:"votes"`
	}
	if err := json.Unmarshal([]byte(a.body), &round); err != nil || round.IsRevealed == nil || round.Votes == nil {
		return PokerRound{}, unrecognized(pokerStepApp, err)
	}
	out := PokerRound{Revealed: *round.IsRevealed, Votes: make([]PokerVote, 0, len(*round.Votes))}
	for _, v := range *round.Votes {
		if v.UserID == "" || v.Vote == nil {
			return PokerRound{}, unrecognized(pokerStepApp, nil)
		}
		out.Votes = append(out.Votes, PokerVote{Account: v.UserID, Vote: *v.Vote})
	}
	return out, nil
}

// CastPokerVote casts or changes the caller's vote on issue, a numeric
// issue ID. It writes no estimate.
func (c *Client) CastPokerVote(ctx context.Context, issue string, vote float64) error {
	return c.pokerWrite(ctx, issue, http.MethodPut, "planningPoker/vote", map[string]any{"issueId": issue, "vote": vote})
}

// RevealPokerVotes shows every vote on issue, a numeric issue ID.
func (c *Client) RevealPokerVotes(ctx context.Context, issue string) error {
	return c.pokerWrite(ctx, issue, http.MethodPut, "planningPoker/reveal/"+issue, nil)
}

// ClearPokerVotes removes every vote on issue, a numeric issue ID.
func (c *Client) ClearPokerVotes(ctx context.Context, issue string) error {
	return c.pokerWrite(ctx, issue, http.MethodDelete, "planningPoker/votes/"+issue, nil)
}

// pokerWrite sends one write to the poker app: applied on a 2xx, refused
// (*PokerError Refused) on a 4xx, and otherwise of unknown outcome unless
// its error proves it not sent.
func (c *Client) pokerWrite(ctx context.Context, issue, method, path string, body any) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	if err := checkID("issue", issue); err != nil {
		return err
	}
	_, err := c.pokerInvoke(ctx, issue, method, path, body, false)
	return err
}

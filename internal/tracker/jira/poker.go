package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
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
// of the invoke's answer (pokerInvoke).
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

// gatewayMessage is errs' first message, cut to maxGatewayMessage bytes.
func gatewayMessage(errs []gqlError) string {
	if len(errs) == 0 {
		return ""
	}
	msg := errs[0].Message
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
		return pokerApp{}, &PokerError{Step: pokerStepListing, Detail: "refused the query: " + gatewayMessage(out.Errors)}
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

// sealInvoke is err, the invoke's failed request, carrying nothing of the
// invoke's answer, which may repeat the token: a status the gateway
// answered is a PokerError that wraps no *trackerapi.StatusError (its
// Body, Message and Fields are the answer's), a refused credential has no
// Message (the answer's), and an answer that did not decode is a
// PokerError that wraps no decode error. Every other failure (a rate
// limit, the network, an ended call, a request not sent) holds no text
// of the answer and is pokerCallErr's.
func sealInvoke(err error) error {
	if se, ok := outcomeOf(err).(*trackerapi.StatusError); ok {
		return &PokerError{Step: pokerStepGateway, Detail: fmt.Sprintf("answered HTTP %d", se.Status), Refused: clientStatus(se.Status)}
	}
	if _, ok := errors.AsType[*AnswerError](err); ok {
		return unrecognized(pokerStepGateway, nil)
	}
	if ae, ok := err.(*trackerapi.AuthError); ok {
		sealed := *ae
		sealed.Message = ""
		err = &sealed
	}
	return pokerCallErr(pokerStepGateway, err)
}

// pokerInvoke sends the poker app method and path (under
// pokerRoutesRoot) for issue, with body, read marking a call that changes
// nothing, and decodes the poker app's 2xx body into into when into is
// not nil. A site without the add-on fails with ErrPokerAbsent before any
// request of the token or the invoke; every failure before the invoke is
// sent is a *trackerapi.NotSentError, so a write it stops is known not
// sent.
//
// Nothing pokerInvoke returns holds the context token, which the gateway
// and the poker app may repeat however they spell it: errors carry no
// text of the answer but its decoded strings (sealInvoke), and once the
// answer and into are decoded, an answer any of whose decoded strings
// holds the token is refused whole (tokenEchoed), its text and into
// unused.
func (c *Client) pokerInvoke(ctx context.Context, issue, method, path string, body any, read bool, into any) error {
	app, err := c.pokerApp(ctx)
	switch {
	case err != nil:
		return &trackerapi.NotSentError{Err: err}
	case app.ext == nil:
		return &trackerapi.NotSentError{Err: ErrPokerAbsent}
	}
	tok, err := c.pokerToken(ctx, app, issue)
	if err != nil {
		return &trackerapi.NotSentError{Err: err}
	}
	token := string(tok.token.Reveal())
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
			"contextToken": token,
		},
	}
	var out invokeAnswer
	req := trackerapi.Request{Method: http.MethodPost, Path: gatewayPath, ReadOnly: read,
		Body: map[string]any{"operationName": "Invoke", "query": pokerInvokeDoc, "variables": map[string]any{"input": input}}}
	if _, err := c.call(ctx, req, &out); err != nil {
		return sealInvoke(err)
	}
	err = out.outcome(into)
	if out.refusedCall() {
		c.pokerTokens.drop(issue)
	}
	// The one check of what leaves: after the last decode, on every string
	// the answer and into decoded to.
	if decodedHolds(reflect.ValueOf(&out), token) || into != nil && decodedHolds(reflect.ValueOf(into), token) {
		return tokenEchoed(err)
	}
	return err
}

// invokeAnswer is the gateway's answer to an invoke.
type invokeAnswer struct {
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

// refusedCall reports an answer in which the gateway refused the invoke,
// so its token is not used again.
func (a *invokeAnswer) refusedCall() bool {
	return a.Data == nil || a.Data.InvokeExtension == nil || !a.Data.InvokeExtension.Success
}

// outcome is the invoke's result as the answer gives it: nil on the poker
// app's 2xx, with its body decoded into into when into is not nil, and
// otherwise the PokerError of the step that refused or failed.
func (a *invokeAnswer) outcome(into any) error {
	if a.Data == nil || a.Data.InvokeExtension == nil {
		if len(a.Errors) > 0 {
			return &PokerError{Step: pokerStepGateway, Detail: "refused the call: " + gatewayMessage(a.Errors)}
		}
		return unrecognized(pokerStepGateway, nil)
	}
	inv := a.Data.InvokeExtension
	if !inv.Success {
		refused := len(inv.Errors) > 0 && inv.Errors[0].Extensions != nil && clientStatus(inv.Errors[0].Extensions.StatusCode)
		return &PokerError{Step: pokerStepGateway, Detail: "refused the call: " + gatewayMessage(inv.Errors), Refused: refused}
	}
	if inv.Response == nil || inv.Response.Body == nil || inv.Response.Body.Body == nil || inv.Response.Body.Status == 0 {
		return unrecognized(pokerStepApp, nil)
	}
	status, text := inv.Response.Body.Status, inv.Response.Body.StatusText
	line := fmt.Sprint(status)
	if text != "" {
		line += " " + text
	}
	switch {
	case status >= 200 && status <= 299:
		if into != nil && json.Unmarshal([]byte(*inv.Response.Body.Body), into) != nil {
			return unrecognized(pokerStepApp, nil)
		}
		return nil
	case clientStatus(status):
		return &PokerError{Step: pokerStepApp, Detail: "refused it (HTTP " + line + ")", Refused: true}
	}
	return &PokerError{Step: pokerStepApp, Detail: "failed (HTTP " + line + ")"}
}

// tokenEchoed is err, the outcome of an answer that repeats the context
// token, as pokerInvoke returns it: a PokerError of the same step and
// refusal with none of the answer's text. An answer that was otherwise
// accepted is the poker app's, and not one the client can use.
func tokenEchoed(err error) error {
	step, refused := pokerStepApp, false
	if pe, ok := errors.AsType[*PokerError](err); ok {
		step, refused = pe.Step, pe.Refused
	}
	return &PokerError{Step: step, Detail: "answered with the context token in its answer", Refused: refused}
}

// decodedHolds reports whether any string v holds, a decoded answer's
// fields, elements, map keys and values, contains token.
func decodedHolds(v reflect.Value, token string) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.Contains(v.String(), token)
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && decodedHolds(v.Elem(), token)
	case reflect.Struct:
		for i := range v.NumField() {
			if decodedHolds(v.Field(i), token) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if decodedHolds(v.Index(i), token) {
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if decodedHolds(it.Key(), token) || decodedHolds(it.Value(), token) {
				return true
			}
		}
	}
	return false
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
	var round struct {
		IsRevealed *bool `json:"isRevealed"`
		Votes      *[]struct {
			UserID string   `json:"userId"`
			Vote   *float64 `json:"vote"`
		} `json:"votes"`
	}
	if err := c.pokerInvoke(ctx, issue, http.MethodGet, "planningPoker/"+issue, nil, true, &round); err != nil {
		return PokerRound{}, err
	}
	if round.IsRevealed == nil || round.Votes == nil {
		return PokerRound{}, unrecognized(pokerStepApp, nil)
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
	return c.pokerInvoke(ctx, issue, method, path, body, false, nil)
}

// Package claudefake answers Claude's OAuth profile and usage endpoints in
// process, for tests and isolated boots, so account identification and usage
// readings run through the real client without a network.
//
// The real endpoint knows who a token belongs to because it issued it. The
// fake knows because the token says so: AccessToken spells the identity into
// the bearer, and ServeProfile reads it back. Any other bearer is answered as
// the real server answers a token it never issued, with 401. Keeping identity
// in the token, never in a config file beside it, is what lets a test pair one
// account's `oauthAccount` record with another account's credential and see
// AO identify the credential.
package claudefake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agent-overflow/internal/provider/claude"
)

const tokenPrefix = "mock-profile:"

// AccessToken returns a bearer the fake profile endpoint answers as email on
// organization orgID. nonce distinguishes successive tokens of one login, as a
// rotation does. The grammar is `mock-profile:<email>:<orgID>:<nonce>`; the
// e2e suite's provider-account helpers spell the same one.
func AccessToken(email, orgID, nonce string) string {
	if email == "" || orgID == "" || strings.Contains(email, ":") || strings.Contains(orgID, ":") {
		panic(fmt.Sprintf("claudefake: identity %q/%q cannot be spelled into a token", email, orgID))
	}
	return tokenPrefix + email + ":" + orgID + ":" + nonce
}

// OrgName is the organization name the fake reports for orgID.
func OrgName(orgID string) string {
	return "Org " + orgID
}

// IsProfile reports whether req is a profile request this fake answers.
func IsProfile(req *http.Request) bool {
	return req.Method == http.MethodGet && req.URL.String() == claude.ProfileURL
}

// ServeProfile answers one profile request from its bearer.
func ServeProfile(req *http.Request) (*http.Response, error) {
	email, orgID, ok := bearerIdentity(req)
	if !ok {
		return unauthorized(req), nil
	}
	body, err := json.Marshal(map[string]any{
		"account":      map[string]string{"uuid": "mock-account-" + email, "email": email},
		"organization": map[string]string{"uuid": orgID, "name": OrgName(orgID)},
	})
	if err != nil {
		return nil, fmt.Errorf("claudefake: encode profile: %w", err)
	}
	return response(req, http.StatusOK, string(body)), nil
}

// bearerIdentity reads the email and organization a fake token spells.
func bearerIdentity(req *http.Request) (email, orgID string, ok bool) {
	bearer, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(bearer, tokenPrefix)
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func unauthorized(req *http.Request) *http.Response {
	return response(req, http.StatusUnauthorized, `{"error":{"type":"authentication_error"}}`)
}

// Transport answers profile requests and refuses everything else, for tests
// that need no other Claude endpoint.
type Transport struct{}

func (Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if IsProfile(req) {
		return ServeProfile(req)
	}
	return nil, fmt.Errorf("claudefake: no fake for %s %s", req.Method, req.URL)
}

func response(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Request:    req,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
}

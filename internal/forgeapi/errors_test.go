package forgeapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClassifyCoversEveryRowOfTheErrorTable(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(10 * time.Minute)
	hdr := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(pairs); i += 2 {
			h.Set(pairs[i], pairs[i+1])
		}
		return h
	}
	exhausted := hdr("X-RateLimit-Limit", "5000", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10), "X-RateLimit-Resource", "core")
	tests := []struct {
		name    string
		status  int
		header  http.Header
		body    string
		graphQL bool
		want    outcome
		until   time.Time // for rate limited; zero means the gate's fallback
		check   func(t *testing.T, err error)
	}{
		{name: "429 with Retry-After seconds", status: 429, header: hdr("Retry-After", "30"), want: outcomeRateLimited, until: now.Add(30 * time.Second)},
		{name: "429 with Retry-After date", status: 429, header: hdr("Retry-After", now.Add(2*time.Minute).UTC().Format(http.TimeFormat)), want: outcomeRateLimited, until: now.Add(2 * time.Minute)},
		{name: "429 with GitLab reset", status: 429, header: hdr("RateLimit-Limit", "2000", "RateLimit-Remaining", "0", "RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10)), want: outcomeRateLimited, until: reset},
		{name: "429 bare", status: 429, header: hdr(), want: outcomeRateLimited},
		{name: "403 remaining zero", status: 403, header: exhausted, body: `{"message":"API rate limit exceeded"}`, want: outcomeRateLimited, until: reset},
		{name: "403 with Retry-After (secondary limit)", status: 403, header: hdr("Retry-After", "60"), want: outcomeRateLimited, until: now.Add(time.Minute)},
		{name: "403 body names the rate limit", status: 403, header: hdr(), body: `{"message":"You have exceeded a secondary Rate Limit"}`, want: outcomeRateLimited},
		{name: "403 plain (SSO or scope)", status: 403, header: hdr("X-RateLimit-Limit", "5000", "X-RateLimit-Remaining", "4000"),
			body: `{"message":"Resource protected by organization SAML enforcement"}`, want: outcomeStatus,
			check: func(t *testing.T, err error) {
				var status *StatusError
				if !errors.As(err, &status) || status.Status != 403 || !strings.Contains(status.Body, "SAML") {
					t.Fatalf("err = %#v, want a 403 *StatusError with the body", err)
				}
				if _, setup := errors.AsType[*SetupError](err); setup {
					t.Fatal("a plain 403 read as a setup error")
				}
			}},
		{name: "GraphQL 200 with RATE_LIMITED", status: 200, graphQL: true, header: hdr(), body: `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, want: outcomeRateLimited},
		{name: "GraphQL 200 errors beside remaining zero", status: 200, graphQL: true,
			header: hdr("X-RateLimit-Limit", "5000", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10), "X-RateLimit-Resource", "graphql"),
			body:   `{"errors":[{"message":"something"}]}`, want: outcomeRateLimited, until: reset},
		// GitHub's GraphQL documentation: a secondary rate limit answers
		// "200 or 403" with an error message saying so; its type and body
		// are undocumented, so the message and Retry-After decide.
		{name: "GraphQL 200 secondary limit with Retry-After", status: 200, graphQL: true, header: hdr("Retry-After", "90"),
			body: `{"errors":[{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}]}`, want: outcomeRateLimited, until: now.Add(90 * time.Second)},
		{name: "GraphQL 200 secondary limit message alone", status: 200, graphQL: true, header: hdr(),
			body: `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit."}]}`, want: outcomeRateLimited},
		{name: "GraphQL 200 errors beside Retry-After", status: 200, graphQL: true, header: hdr("Retry-After", "30"),
			body: `{"errors":[{"message":"something"}]}`, want: outcomeRateLimited, until: now.Add(30 * time.Second)},
		{name: "401", status: 401, header: hdr(), body: `{"message":"Bad credentials"}`, want: outcomeUnauthorized},
		{name: "404", status: 404, header: hdr(), body: `{"message":"Not Found"}`, want: outcomeNotFound,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("404 %v does not match ErrNotFound", err)
				}
			}},
		{name: "304", status: 304, header: hdr(), want: outcomeNotModified},
		{name: "500", status: 500, header: hdr(), body: "oops", want: outcomeStatus,
			check: func(t *testing.T, err error) {
				if errors.Is(err, ErrNotFound) {
					t.Fatal("a 500 matched ErrNotFound")
				}
			}},
		{name: "GraphQL errors", status: 200, graphQL: true, header: hdr(), body: `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve","path":["repository"]}]}`, want: outcomeGraphQL,
			check: func(t *testing.T, err error) {
				var gql *GraphQLError
				if !errors.As(err, &gql) || len(gql.Problems) != 1 || gql.Problems[0].Type != "NOT_FOUND" || string(gql.Data) != `{"repository":null}` {
					t.Fatalf("err = %#v", err)
				}
			}},
		{name: "GraphQL success", status: 200, graphQL: true, header: hdr(), body: `{"data":{"viewer":{"login":"x"}}}`, want: outcomeOK},
		{name: "REST success", status: 200, header: hdr(), body: `{}`, want: outcomeOK},
		{name: "REST 200 mentioning rate limit is not limited", status: 200, header: hdr(), body: `{"title":"rate limit docs"}`, want: outcomeOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(answer{host: "github.com", pool: "core", method: "GET", url: "https://api.github.com/x", status: tc.status, header: tc.header, body: []byte(tc.body), graphQL: tc.graphQL, now: now})
			if got.outcome != tc.want {
				t.Fatalf("outcome = %v (err %v), want %v", got.outcome, got.err, tc.want)
			}
			if tc.want == outcomeRateLimited {
				limited, ok := got.err.(*RateLimitedError)
				if !ok {
					t.Fatalf("err = %T, want *RateLimitedError", got.err)
				}
				if !limited.Until.Equal(tc.until) {
					t.Fatalf("Until = %v, want %v", limited.Until, tc.until)
				}
				if limited.Host != "github.com" || limited.Pool != "core" || limited.Reserve {
					t.Fatalf("limited = %+v", limited)
				}
			}
			if (tc.want == outcomeOK || tc.want == outcomeNotModified) && got.err != nil {
				t.Fatalf("success carried err %v", got.err)
			}
			if tc.check != nil {
				tc.check(t, got.err)
			}
		})
	}
}

func TestStatusErrorBodyIsBounded(t *testing.T) {
	t.Parallel()
	got := classify(answer{method: "GET", url: "u", status: 502, header: http.Header{}, body: []byte(strings.Repeat("x", 10_000))})
	var status *StatusError
	if !errors.As(got.err, &status) || len(status.Body) != maxErrorBody {
		t.Fatalf("body length = %d, want %d", len(status.Body), maxErrorBody)
	}
}

func TestParseRateLimitNamesThePool(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Remaining", "12")
	h.Set("X-RateLimit-Reset", "1700000600")
	h.Set("X-RateLimit-Resource", "graphql")
	rate := parseRateLimit(h, "core")
	if rate == nil || rate.Pool != "graphql" || rate.Limit != 5000 || rate.Remaining != 12 || rate.Reset.Unix() != 1700000600 {
		t.Fatalf("rate = %+v", rate)
	}
	gitlab := http.Header{}
	gitlab.Set("RateLimit-Limit", "2000")
	gitlab.Set("RateLimit-Remaining", "1999")
	gitlab.Set("RateLimit-Name", "throttle_authenticated_api")
	if rate := parseRateLimit(gitlab, "throttle_authenticated_api"); rate == nil || rate.Pool != "throttle_authenticated_api" || rate.Remaining != 1999 {
		t.Fatalf("gitlab rate = %+v", rate)
	}
	if parseRateLimit(http.Header{}, "core") != nil {
		t.Fatal("no headers parsed as a quota")
	}
}

// A 403 names a scope only with evidence the token lacks it: GitLab's
// insufficient_scope, or GitHub's accepted scopes beside granted ones none
// of which is accepted. X-Accepted-OAuth-Scopes alone names what the
// endpoint checks, and a fine-grained token's refusal is a permission,
// not a classic scope.
func TestForbiddenErrorScopeNeedsEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		header  map[string]string
		body    string
		scope   string
		message string
	}{
		{name: "GitHub fully scoped token refused", header: map[string]string{"X-Accepted-OAuth-Scopes": "repo", "X-OAuth-Scopes": "repo, read:org"},
			body: `{"message":"Resource protected by organization SAML enforcement"}`, message: "Resource protected by organization SAML enforcement"},
		{name: "GitHub token missing the scope", header: map[string]string{"X-Accepted-OAuth-Scopes": "repo", "X-OAuth-Scopes": "read:user"},
			body: `{"message":"Not allowed"}`, scope: "repo", message: "Not allowed"},
		{name: "GitHub accepted scopes without granted ones", header: map[string]string{"X-Accepted-OAuth-Scopes": "repo"},
			body: `{"message":"Must have admin rights to Repository."}`, message: "Must have admin rights to Repository."},
		{name: "GitHub endpoint that checks no scope", header: map[string]string{"X-Accepted-OAuth-Scopes": "", "X-OAuth-Scopes": "read:user"},
			body: `{"message":"Forbidden"}`, message: "Forbidden"},
		{name: "GitHub fine-grained token", header: map[string]string{"X-Accepted-OAuth-Scopes": "repo", "X-OAuth-Scopes": ""},
			body: `{"message":"Resource not accessible by personal access token"}`, message: "Resource not accessible by personal access token"},
		{name: "GitLab insufficient scope", body: `{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token.","scope":"api read_user"}`,
			scope: "api read_user", message: "The request requires higher privileges than provided by the access token."},
		{name: "GitLab insufficient scope naming none", body: `{"error":"insufficient_scope"}`, scope: "api"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			for k, v := range tc.header {
				header[http.CanonicalHeaderKey(k)] = []string{v}
			}
			got := classify(answer{method: "PUT", url: "https://api.example/x", status: http.StatusForbidden, header: header, body: []byte(tc.body)})
			forbidden, ok := got.err.(*ForbiddenError)
			if !ok {
				t.Fatalf("err = %T %v, want *ForbiddenError", got.err, got.err)
			}
			if forbidden.Scope != tc.scope || forbidden.Message != tc.message {
				t.Fatalf("scope %q message %q, want %q %q", forbidden.Scope, forbidden.Message, tc.scope, tc.message)
			}
		})
	}
}

// GitHub refuses a token not authorized for an organization's SAML single
// sign-on with 403 or 404; the 403's X-GitHub-SSO carries the URL that
// authorizes it (GitHub's REST authentication documentation). Either
// status with the header is a refusal of the login, never a resource
// that is gone; a GraphQL answer with the header carries the URL too.
func TestSSORefusalsCarryTheAuthorizeURL(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/orgs/acme/sso?authorization_request=A1B2"
	sso := http.Header{}
	sso.Set("X-GitHub-SSO", "required; url="+url)
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		got := classify(answer{method: "GET", url: "https://api.github.com/repos/acme/app", status: status, header: sso, body: []byte(`{"message":"Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."}`)})
		forbidden, ok := got.err.(*ForbiddenError)
		if !ok || !forbidden.SSO || forbidden.SSOURL != url {
			t.Fatalf("HTTP %d with X-GitHub-SSO = %#v, want a ForbiddenError with the URL", status, got.err)
		}
		if errors.Is(got.err, ErrNotFound) {
			t.Fatalf("HTTP %d SSO refusal matched ErrNotFound", status)
		}
	}
	partial := http.Header{}
	partial.Set("X-GitHub-SSO", "partial-results; organizations=21955855,20582480")
	if got := classify(answer{method: "GET", url: "u", status: http.StatusNotFound, header: partial}); !errors.Is(got.err, ErrNotFound) {
		t.Fatalf("a 404 whose header only reports partial results = %#v, want not found", got.err)
	}
	got := classify(answer{method: "POST", url: "https://api.github.com/graphql", graphQL: true, status: 200, header: sso,
		body: []byte(`{"data":{"repository":null},"errors":[{"type":"FORBIDDEN","path":["repository"],"message":"Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."}]}`)})
	gql, ok := got.err.(*GraphQLError)
	if !ok || !gql.SSO || gql.SSOURL != url || !gql.Forbidden() {
		t.Fatalf("GraphQL SSO refusal = %#v", got.err)
	}
}

// A 403 a fine-grained token gets for a permission it lacks names the
// permissions the endpoint accepts (X-Accepted-GitHub-Permissions), and
// an organization's token policy is told from other refusals.
func TestForbiddenErrorNamesTheTokenCause(t *testing.T) {
	t.Parallel()
	header := http.Header{}
	header.Set("X-Accepted-GitHub-Permissions", "pull_requests=write")
	got := classify(answer{method: "POST", url: "u", status: http.StatusForbidden, header: header, body: []byte(`{"message":"Resource not accessible by personal access token"}`)})
	forbidden, ok := got.err.(*ForbiddenError)
	if !ok || !forbidden.FineGrained() || forbidden.Permissions != "pull_requests=write" || forbidden.Policy() {
		t.Fatalf("fine-grained refusal = %#v", got.err)
	}
	got = classify(answer{method: "GET", url: "u", status: http.StatusForbidden, header: http.Header{}, body: []byte(`{"message":"The 'acme' organization forbids access via a personal access tokens (classic). Please use a GitHub App, OAuth App, or a personal access token with fine-grained permissions."}`)})
	if forbidden, ok = got.err.(*ForbiddenError); !ok || !forbidden.Policy() || forbidden.FineGrained() {
		t.Fatalf("policy refusal = %#v", got.err)
	}
	gql := &GraphQLError{Problems: []GraphQLProblem{{Type: "NOT_FOUND", Message: "x"}}}
	if gql.Forbidden() {
		t.Fatal("NOT_FOUND read as forbidden")
	}
	gql = &GraphQLError{Problems: []GraphQLProblem{{Type: "INSUFFICIENT_SCOPES", Message: "Your token has not been granted the required scopes to execute this query. The 'combinedSlug' field requires one of the following scopes: ['read:org'], but your token has only been granted the: ['repo'] scopes."}}}
	if !gql.Forbidden() || gql.MissingScope() != "read:org" {
		t.Fatalf("INSUFFICIENT_SCOPES = forbidden %v, scope %q", gql.Forbidden(), gql.MissingScope())
	}
}

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

package tempo

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

func TestTransportOptions(t *testing.T) {
	issues := newIssues()
	opts, err := transportOptions(Options{Version: "1.0", Issues: issues})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name != "tempo" || opts.BaseURL != DefaultBaseURL || opts.MaxPerSecond != 5 || opts.Version != "1.0" {
		t.Errorf("options %+v", opts)
	}
	if _, ok := opts.Dialect.(dialect); !ok {
		t.Errorf("dialect %T", opts.Dialect)
	}
	opts, err = transportOptions(Options{BaseURL: EUBaseURL, Version: "1.0", Issues: issues})
	if err != nil || opts.BaseURL != "https://api.eu.tempo.io" {
		t.Errorf("regional base %q, %v", opts.BaseURL, err)
	}
	if _, err := New(Options{Version: "1.0", Credentials: trackerapi.StaticCredentials(nil)}); err == nil {
		t.Error("New accepted options without Issues")
	}
	if _, err := New(Options{BaseURL: "http://example.com", Version: "1.0", Credentials: trackerapi.StaticCredentials(nil), Issues: issues}); err == nil {
		t.Error("New accepted a plain http base URL")
	}
	c, err := New(Options{Version: "1.0", Credentials: trackerapi.StaticCredentials(nil), Issues: issues})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c.Close()
}

func TestDialectProblem(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"list", `{"errors":[{"message":"Work attribute 'Category' is required"},{"message":" Issue not found "}]}`, "Work attribute 'Category' is required; Issue not found"},
		{"object", `{"errors":{"message":"'date' must be valid on the format: 'yyyy-MM-dd'"}}`, "'date' must be valid on the format: 'yyyy-MM-dd'"},
		{"html", `<html><body>Unauthorized</body></html>`, ""},
		{"other json", `{"message":"x"}`, ""},
		{"empty list", `{"errors":[]}`, ""},
	}
	for _, tc := range cases {
		p := dialect{}.Problem(400, []byte(tc.body))
		if p.Message != tc.want || len(p.Fields) != 0 {
			t.Errorf("%s: %+v, want %q", tc.name, p, tc.want)
		}
	}
	if (dialect{}).RejectedLogin(http.Header{"X-Seraph-Loginreason": {"AUTHENTICATED_FAILED"}}) {
		t.Error("RejectedLogin marked a header Tempo does not send")
	}
	if _, limited := (dialect{}).RateLimited(503, http.Header{}, nil); limited {
		t.Error("RateLimited marked a 503")
	}
}

func TestErrorClasses(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	ctx := context.Background()

	f.on("GET", "/4/worklogs/5", nil, "", 400, `{"errors":[{"message":"bad id"}]}`)
	_, err := c.Worklog(ctx, 5)
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok || se.Status != 400 || se.Message != "bad id" {
		t.Fatalf("400: %v", err)
	}

	f.on("GET", "/4/worklogs/6", nil, "", 404, `{"errors":{"message":"Worklog not found"}}`)
	if _, err := c.Worklog(ctx, 6); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}

	// A 401 is read again once, then refused.
	f.on("GET", "/4/worklogs/7", nil, "", 401, `<html>Unauthorized</html>`).repeat = true
	if _, err := c.Worklog(ctx, 7); !isType[*trackerapi.AuthError](err) {
		t.Fatalf("401: %v", err)
	}

	if _, err := c.Worklog(ctx, 0); !isType[*trackerapi.NotSentError](err) {
		t.Fatalf("ID 0: %v", err)
	}
}

func TestRateLimitClosesGate(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	e := f.on("GET", "/4/worklogs/8", nil, "", 429, `{"errors":[{"message":"Too many requests"}]}`)
	e.header = http.Header{"Retry-After": {"30"}}
	before := time.Now()
	_, err := c.Worklog(context.Background(), 8)
	limited, ok := errors.AsType[*trackerapi.RateLimitedError](err)
	if !ok || limited.Status != 429 || limited.Until.Before(before.Add(29*time.Second)) {
		t.Fatalf("429: %v", err)
	}
	// The gate is closed: the next background call fails without a
	// request, which the fake would refuse.
	_, err = c.Worklog(context.Background(), 9)
	if limited, ok := errors.AsType[*trackerapi.RateLimitedError](err); !ok || limited.Status != 0 || !isType[*trackerapi.NotSentError](err) {
		t.Fatalf("gate: %v", err)
	}
	// A write refused by the gate was not applied.
	out := c.Apply(context.Background(), []Op{DeleteOp(9)})[0]
	if out.Status != Refused || !trackerapi.WriteNotApplied(out.Err) {
		t.Fatalf("delete behind the gate: %+v", out)
	}
}

func TestCloseRefusesLaterCalls(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	c.Close()
	if _, err := c.Attributes(context.Background()); !isType[*trackerapi.NotSentError](err) {
		t.Fatalf("attributes after Close: %v", err)
	}
	if _, err := c.Worklog(context.Background(), 1); !isType[*trackerapi.NotSentError](err) {
		t.Fatalf("worklog after Close: %v", err)
	}
}

func isType[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}

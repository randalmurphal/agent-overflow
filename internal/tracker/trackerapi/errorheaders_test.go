package trackerapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// A StatusError keeps the headers ErrorHeaders names and no other, each
// value scrubbed, in a map of its own; a client that names none keeps
// none.
func TestStatusErrorKeepsNamedHeaders(t *testing.T) {
	t.Parallel()
	echo := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Account", "user-1")
			w.Header()["X-Echo"] = []string{"first", "you sent " + r.Header.Get("Authorization")}
			w.Header().Set("X-Other", "unasked")
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(status)
			writeString(w, `{}`)
		}
	}
	tr := newTracker(t, route{
		"GET /missing": echo(http.StatusNotFound),
		"GET /broken":  echo(http.StatusInternalServerError),
		"POST /moved":  echo(http.StatusFound),
		"GET /bare":    reply(http.StatusNotFound),
	})
	opts := testOptions(tr.url())
	opts.ErrorHeaders = []string{"x-account", "X-Echo", "X-Absent"}
	c, _ := loopbackClient(t, opts)
	for _, req := range []Request{
		{Path: "missing"},
		{Path: "broken"},
		{Method: http.MethodPost, Path: "moved", Body: map[string]string{}},
	} {
		_, err := c.Do(t.Context(), req)
		failure, ok := errors.AsType[*StatusError](err)
		if !ok {
			t.Fatalf("%s: Do = %v, want a *StatusError", req.Path, err)
		}
		if got := failure.Header.Get("X-Account"); got != "user-1" {
			t.Errorf("%s: X-Account = %q, want it as sent", req.Path, got)
		}
		echoed := failure.Header.Values("X-Echo")
		if len(echoed) != 2 || echoed[0] != "first" || echoed[1] != "you sent "+secret.Redacted {
			t.Errorf("%s: X-Echo = %q, want both values, the credential replaced", req.Path, echoed)
		}
		if len(failure.Header) != 2 {
			t.Errorf("%s: kept %v, want only the named headers the answer holds", req.Path, failure.Header)
		}
		if strings.Contains(failure.Error(), "user-1") {
			t.Errorf("%s: the error text %q quotes a kept header", req.Path, failure.Error())
		}
	}
	_, err := c.Do(t.Context(), Request{Path: "bare"})
	if failure, ok := errors.AsType[*StatusError](err); !ok || failure.Header == nil || len(failure.Header) != 0 {
		t.Fatalf("an answer with none of the named headers: %v, want an empty map", err)
	}

	plain, _ := loopbackClient(t, testOptions(tr.url()))
	_, err = plain.Do(t.Context(), Request{Path: "missing"})
	if failure, ok := errors.AsType[*StatusError](err); !ok || failure.Header != nil {
		t.Fatalf("a client that names no ErrorHeaders: %v, want no Header", err)
	}
}

package startupprogress

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteAndParseRoundTripOnlyAStartingReport(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, Progress{Phase: "store.migrate", Step: 2, Steps: 5, UpdatedAt: 9, UpdatingTo: "3.0.0"})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || rec.Header().Get("Cache-Control") != "no-store, max-age=0" {
		t.Fatalf("response = %d %v, want the uncacheable 503 with Retry-After", rec.Code, rec.Header())
	}
	got, ok := Parse(rec.Code, rec.Body.Bytes())
	if !ok || got != (Progress{Phase: "store.migrate", Step: 2, Steps: 5, UpdatedAt: 9, UpdatingTo: "3.0.0"}) {
		t.Fatalf("round trip = %+v %v", got, ok)
	}
	for _, c := range []struct {
		status int
		body   string
	}{
		{http.StatusServiceUnavailable, "backend not ready\n"},
		{http.StatusServiceUnavailable, `{"reason":"other","phase":"x"}`},
		{http.StatusOK, `{"reason":"starting","phase":"x"}`},
	} {
		if _, ok := Parse(c.status, []byte(c.body)); ok {
			t.Errorf("parsed %d %q as a starting report", c.status, c.body)
		}
	}
}

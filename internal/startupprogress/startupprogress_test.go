package startupprogress

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteAndParseRoundTripOnlyAStartingReport(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, Progress{Phase: "store.migrate", Step: 2, Steps: 5, UpdatedAt: 9, AliveAt: 11, UpdatingTo: "3.0.0"})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || rec.Header().Get("Cache-Control") != "no-store, max-age=0" {
		t.Fatalf("response = %d %v, want the uncacheable 503 with Retry-After", rec.Code, rec.Header())
	}
	got, ok := Parse(rec.Code, rec.Body.Bytes())
	if !ok || got != (Progress{Phase: "store.migrate", Step: 2, Steps: 5, UpdatedAt: 9, AliveAt: 11, UpdatingTo: "3.0.0"}) {
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

func TestStatusNamesTheUpdateBeingFinished(t *testing.T) {
	for _, c := range []struct {
		p    Progress
		want string
	}{
		{Progress{Detail: "Applying migration 3 of 7 add_index"}, "Applying migration 3 of 7 add_index"},
		{Progress{}, "Starting"},
		{Progress{Detail: "Applying migration 3 of 7 add_index", UpdatingTo: "1.2.3"}, "Finishing update to v1.2.3: applying migration 3 of 7 add_index"},
		{Progress{Detail: "Opening the database", UpdatingTo: "v2.0.0"}, "Finishing update to v2.0.0: opening the database"},
		{Progress{Detail: "WSL setup", UpdatingTo: "2.0.0"}, "Finishing update to v2.0.0: WSL setup"},
		{Progress{UpdatingTo: "2.0.0"}, "Finishing update to v2.0.0: starting"},
	} {
		if got := c.p.Status(); got != c.want {
			t.Errorf("Status(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}

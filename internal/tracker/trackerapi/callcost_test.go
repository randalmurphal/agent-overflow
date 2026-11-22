//go:build !race

// The race detector drops pooled objects at random, net/http's among
// them, so allocation counts are pinned only without it.

package trackerapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// callOverheadAllocs is what one Do of a small answer allocates beyond
// net/http's own request on the same server: the call, its contexts and
// timer, the hop's trace, the URL, the credential's header, the answer
// and the Response. BenchmarkCall measures the same.
const callOverheadAllocs = 20

func TestCallAllocations(t *testing.T) {
	body := costPage(1, "")
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	c, err := newClient(testOptions("https://example.atlassian.net/"), srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	ctx := context.Background()
	hc := srv.Client()
	do := func() {
		if resp, err := c.Do(ctx, Request{Path: "rest/api/3/search/jql"}); err != nil || len(resp.Body) != len(body) {
			t.Fatal(err)
		}
	}
	bare := func() {
		resp, err := hc.Get("https://example.atlassian.net/rest/api/3/search/jql")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || len(got) != len(body) {
			t.Fatal(err)
		}
	}
	do()
	bare()
	transport, plain := testing.AllocsPerRun(200, do), testing.AllocsPerRun(200, bare)
	if over := transport - plain; over > callOverheadAllocs {
		t.Errorf("a call allocates %v times, net/http alone %v: %v over, want at most %d", transport, plain, over, callOverheadAllocs)
	}
}

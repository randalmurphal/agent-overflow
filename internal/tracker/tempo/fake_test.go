package tempo

import (
	"context"
	"net/url"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/tempo/tempotest"
	"agent-overflow/internal/tracker/trackerapi"
)

const testToken = "fake-tempo-token"

// fakeIssues is an IssueIDs over fixed maps that counts its calls.
type fakeIssues struct {
	mu       sync.Mutex
	ids      map[string]int64
	keys     map[int64]string
	err      error
	idCalls  [][]string
	keyCalls [][]int64
}

func (f *fakeIssues) IDs(_ context.Context, keys []string) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idCalls = append(f.idCalls, slices.Clone(keys))
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]int64{}
	for _, k := range keys {
		if id, ok := f.ids[k]; ok {
			out[k] = id
		}
	}
	return out, nil
}

func (f *fakeIssues) Keys(_ context.Context, ids []int64) (map[int64]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyCalls = append(f.keyCalls, slices.Clone(ids))
	if f.err != nil {
		return nil, f.err
	}
	out := map[int64]string{}
	for _, id := range ids {
		if k, ok := f.keys[id]; ok {
			out[id] = k
		}
	}
	return out, nil
}

func (f *fakeIssues) calls() (ids [][]string, keys [][]int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.idCalls), slices.Clone(f.keyCalls)
}

func newIssues() *fakeIssues {
	return &fakeIssues{
		ids:  map[string]int64{"WEB-1": 10001, "WEB-2": 10002, "APP-7": 20007},
		keys: map[int64]string{10001: "WEB-1", 10002: "WEB-2", 20007: "APP-7"},
	}
}

// testClient is a Client on f with no pacing, so tests do not wait
// between requests.
func testClient(t *testing.T, f *tempotest.Server, issues IssueIDs) *Client {
	t.Helper()
	opts, err := transportOptions(Options{
		BaseURL:     f.URL(),
		Version:     "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte(testToken)))),
		Issues:      issues,
	})
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxPerSecond = 0
	c, err := newClient(opts, issues)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func mustDate(t *testing.T, s string) Date {
	t.Helper()
	d, err := ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustClock(t *testing.T, s string) TimeOfDay {
	t.Helper()
	c, err := ParseTimeOfDay(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func q(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Add(pairs[i], pairs[i+1])
	}
	return v
}

func ptr[T any](v T) *T { return &v }

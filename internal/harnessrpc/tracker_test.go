package harnessrpc

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// trackerSeed is one fake Jira site of the given host, with User 1.
func trackerSeed(site string) json.RawMessage {
	return json.RawMessage(`{"jira":[{"site":"` + site + `","account":"acc-user1","token":"fake-jira-token-1",
		"users":[{"accountId":"acc-user1","displayName":"User 1","email":"user1@example.com","timeZone":"UTC"}]}]}`)
}

// The fake trackers serve nothing until a seed, then the sites the first
// seed named: the App's tracker clients reach them through Origin, a spec
// through the answer, and every request lands in the log. A reset ends
// the App's tracker connections, the log and the origins, so the next
// seed serves the sites it names.
func TestHarnessTrackersServeTheirSeedAndResetWithTheHarness(t *testing.T) {
	trackers := NewTrackerFake()
	t.Cleanup(trackers.Close)
	h, host := newHarnessTestHost(t)
	h.config.Trackers = trackers

	if _, err := trackers.Origin(store.TrackerKindJira, "https://example.atlassian.net"); err == nil || !strings.Contains(err.Error(), "HarnessTrackerSeed") {
		t.Fatalf("Origin before a seed = %v", err)
	}
	if _, err := h.HarnessTrackerSeed(json.RawMessage(`{"jira":[{"site":"example.atlassian.net","acount":"acc-user1"}]}`)); err == nil {
		t.Fatal("a fixture with an unknown member was accepted")
	}
	origins, err := h.HarnessTrackerSeed(trackerSeed("example.atlassian.net"))
	if err != nil {
		t.Fatal(err)
	}
	jira := origins.Jira["example.atlassian.net"]
	if !strings.HasPrefix(jira, "http://127.0.0.1:") || origins.Tempo == "" || origins.Linear == "" {
		t.Fatalf("seed answered %+v", origins)
	}
	for kind, site := range map[string]string{store.TrackerKindJira: "https://example.atlassian.net", store.TrackerKindTempo: "https://api.tempo.io", store.TrackerKindLinear: "example"} {
		want := map[string]string{store.TrackerKindJira: jira, store.TrackerKindTempo: origins.Tempo, store.TrackerKindLinear: origins.Linear}[kind]
		if got, err := trackers.Origin(kind, site); err != nil || got != want {
			t.Errorf("Origin(%s, %s) = %q, %v, want %q", kind, site, got, err, want)
		}
	}
	if _, err := trackers.Origin(store.TrackerKindJira, "https://other.atlassian.net"); err == nil {
		t.Fatal("Origin answered a Jira site no seed named")
	}
	if _, err := h.HarnessTrackerSeed(trackerSeed("other.atlassian.net")); err == nil {
		t.Fatal("a later seed named a Jira site the servers do not serve")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, jira+"/rest/api/3/myself", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("user1@example.com", "fake-jira-token-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET myself = %d", resp.StatusCode)
	}
	log, err := h.HarnessTrackerRequests(0)
	if err != nil || len(log.Requests) != 1 || log.Requests[0].Path != "/rest/api/3/myself" || log.Requests[0].Status != http.StatusOK {
		t.Fatalf("HarnessTrackerRequests = %+v, %v", log, err)
	}

	if err := h.HarnessReset(); err != nil {
		t.Fatal(err)
	}
	if host.trackerResets != 1 {
		t.Fatalf("the reset removed the App's tracker connections %d times, want 1", host.trackerResets)
	}
	if log, err := h.HarnessTrackerRequests(0); err != nil || len(log.Requests) != 0 {
		t.Fatalf("the request log after a reset = %+v, %v", log, err)
	}
	if _, err := trackers.Origin(store.TrackerKindJira, "https://example.atlassian.net"); err == nil {
		t.Fatal("Origin answered a site the reset dropped")
	}
	// The next seed, of another site, starts the origins it names.
	after, err := h.HarnessTrackerSeed(trackerSeed("other.atlassian.net"))
	if err != nil {
		t.Fatalf("a seed of another site after a reset: %v", err)
	}
	if got, err := trackers.Origin(store.TrackerKindJira, "https://other.atlassian.net"); err != nil || got != after.Jira["other.atlassian.net"] {
		t.Fatalf("Origin of the site seeded after the reset = %q, %v, want %q", got, err, after.Jira["other.atlassian.net"])
	}
	if _, err := trackers.Origin(store.TrackerKindJira, "https://example.atlassian.net"); err == nil {
		t.Fatal("Origin answered the site the seed before the reset named")
	}
}

// A boot without fake trackers (the soak rig) refuses their methods.
func TestHarnessTrackerMethodsNeedTheFake(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	if _, err := h.HarnessTrackerSeed(trackerSeed("example.atlassian.net")); err == nil {
		t.Fatal("HarnessTrackerSeed without fake trackers")
	}
	if _, err := h.HarnessTrackerRequests(0); err == nil {
		t.Fatal("HarnessTrackerRequests without fake trackers")
	}
	if err := h.HarnessReset(); err != nil {
		t.Fatal(err)
	}
}

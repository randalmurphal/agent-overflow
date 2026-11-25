package harnessrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
)

// TrackerFake is a harness boot's issue trackers: one trackerfake engine,
// served on loopback from the first HarnessTrackerSeed after the boot or
// a HarnessReset. The boot gives
// Origin to the App as its tracker origin before Start, so every tracker
// client of the App reaches the fake and nothing else.
type TrackerFake struct {
	engine *trackerfake.Engine

	mu sync.Mutex
	// servers are the engine's origins, started by the first seed since
	// the boot or the last reset with the Jira sites it names; nil before
	// it.
	servers *trackerfake.Servers
}

// NewTrackerFake is an empty fake, serving nothing until a seed.
func NewTrackerFake() *TrackerFake {
	return &TrackerFake{engine: trackerfake.New(trackerfake.Options{})}
}

// Origin is where a tracker client of the App connects for a connection
// of kind on site (trackerapp.Origin): the fake's server for it. Before a
// seed, and for a Jira site the seed that started the servers did not
// name, it refuses.
func (f *TrackerFake) Origin(kind, site string) (string, error) {
	f.mu.Lock()
	servers := f.servers
	f.mu.Unlock()
	if servers == nil {
		return "", errors.New("the harness's fake trackers are not seeded: call HarnessTrackerSeed first")
	}
	switch kind {
	case store.TrackerKindJira:
		host := strings.TrimPrefix(site, "https://")
		if url := servers.JiraURL(host); url != "" {
			return url, nil
		}
		return "", fmt.Errorf("the harness's fake trackers serve no Jira site %s", host)
	case store.TrackerKindTempo:
		return servers.TempoURL(), nil
	case store.TrackerKindLinear:
		return servers.LinearURL(), nil
	}
	return "", fmt.Errorf("the harness's fake trackers have no tracker kind %q", kind)
}

// Close stops the servers and the engine.
func (f *TrackerFake) Close() {
	f.mu.Lock()
	servers := f.servers
	f.mu.Unlock()
	if servers != nil {
		servers.Close()
	}
	f.engine.Close()
}

// TrackerOrigins are the fake trackers' base URLs: each Jira site's by
// its host name, Tempo's and Linear's.
type TrackerOrigins struct {
	Jira   map[string]string `json:"jira"`
	Tempo  string            `json:"tempo"`
	Linear string            `json:"linear"`
}

// seed seeds the engine with fixture and answers where its tenants are
// served, the servers started by the first seed since the boot or the
// last reset with the Jira sites it names. A later seed that names a Jira
// site the servers do not serve is refused, before it seeds anything.
func (f *TrackerFake) seed(fixture trackerfake.Fixture) (TrackerOrigins, error) {
	sites := make([]string, len(fixture.Jira))
	for i, s := range fixture.Jira {
		sites[i] = s.Site
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.servers != nil {
		for _, site := range sites {
			if f.servers.JiraURL(site) == "" {
				return TrackerOrigins{}, fmt.Errorf("the harness's fake trackers serve only the Jira sites the first seed named, and not %s", site)
			}
		}
	}
	if err := f.engine.Seed(fixture); err != nil {
		return TrackerOrigins{}, fmt.Errorf("seed the fake trackers: %w", err)
	}
	if f.servers == nil {
		servers, err := trackerfake.Serve(f.engine, sites...)
		if err != nil {
			f.engine.Reset()
			return TrackerOrigins{}, fmt.Errorf("serve the fake trackers: %w", err)
		}
		f.servers = servers
	}
	out := TrackerOrigins{Jira: map[string]string{}, Tempo: f.servers.TempoURL(), Linear: f.servers.LinearURL()}
	for _, site := range sites {
		out.Jira[site] = f.servers.JiraURL(site)
	}
	return out, nil
}

// HarnessTrackerSeed adds Jira sites, Tempo accounts and Linear
// workspaces to the fake trackers (the fixture format is
// trackerfake.Fixture), replacing any seeded under the same site or
// workspace key, and answers where each is served. The first seed after
// the boot or a HarnessReset fixes the Jira sites served; a later seed
// naming another is refused. HarnessReset clears the seeded tenants, the
// request log and the servers.
func (h *Harness) HarnessTrackerSeed(raw json.RawMessage) (TrackerOrigins, error) {
	if h.config.Trackers == nil {
		return TrackerOrigins{}, errors.New("this harness boot has no fake trackers")
	}
	var fixture trackerfake.Fixture
	if err := decodeStrictJSON("tracker fixture", raw, &fixture); err != nil {
		return TrackerOrigins{}, err
	}
	return h.config.Trackers.seed(fixture)
}

// HarnessTrackerRequests returns the fake trackers' recorded requests
// numbered after since, credentials redacted.
func (h *Harness) HarnessTrackerRequests(since int) (trackerfake.RequestLog, error) {
	if h.config.Trackers == nil {
		return trackerfake.RequestLog{}, errors.New("this harness boot has no fake trackers")
	}
	return h.config.Trackers.engine.Requests(since), nil
}

// HarnessTrackerFault arms a fault control on the fake trackers
// (trackerfake.Fault), after every one armed: a hold keeps the requests it
// matches unanswered until HarnessTrackerRelease, and the log shows each
// as held meanwhile. HarnessReset disarms them.
func (h *Harness) HarnessTrackerFault(raw json.RawMessage) error {
	if h.config.Trackers == nil {
		return errors.New("this harness boot has no fake trackers")
	}
	var fault trackerfake.Fault
	if err := decodeStrictJSON("tracker fault", raw, &fault); err != nil {
		return err
	}
	return h.config.Trackers.engine.AddFault(fault)
}

// TrackerRelease names the holds HarnessTrackerRelease ends: those armed
// on exactly Origin and Route. Drop closes their requests' connections
// with nothing applied instead of answering them.
type TrackerRelease struct {
	Origin trackerfake.Origin `json:"origin"`
	Route  string             `json:"route"`
	Drop   bool               `json:"drop,omitempty"`
}

// HarnessTrackerRelease ends the holds a release names: their faults stop
// matching, and the requests they keep are answered as usual. A release
// naming no armed hold is refused.
func (h *Harness) HarnessTrackerRelease(raw json.RawMessage) error {
	if h.config.Trackers == nil {
		return errors.New("this harness boot has no fake trackers")
	}
	var release TrackerRelease
	if err := decodeStrictJSON("tracker release", raw, &release); err != nil {
		return err
	}
	return h.config.Trackers.engine.Release(release.Origin, release.Route, release.Drop)
}

// resetTrackers ends the App's tracker connections, then empties the fake
// trackers: no connection, subscription or request of one test reaches
// the next.
func (h *Harness) resetTrackers() error {
	if h.config.Host != nil {
		if err := h.config.Host.ResetTrackerConnections(); err != nil {
			return fmt.Errorf("remove tracker connections: %w", err)
		}
	}
	if h.config.Trackers != nil {
		h.config.Trackers.reset()
	}
	return nil
}

// reset empties the engine and stops the servers, so the next seed starts
// the origins it names. The App's tracker connections are gone first
// (resetTrackers), so no client of them is left to reach the servers.
// It holds mu throughout, so a seed lands before it or after it whole.
func (f *TrackerFake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.servers != nil {
		f.servers.Close()
		f.servers = nil
	}
	f.engine.Reset()
}

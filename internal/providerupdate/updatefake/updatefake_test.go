package updatefake_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/providerupdate"
	"agent-overflow/internal/providerupdate/updatefake"
)

var errRefused = errors.New("not faked")

type feedTransport struct{ feed *updatefake.Feed }

func (t feedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if resp, ok, err := t.feed.Serve(req); ok {
		return resp, err
	}
	return nil, errRefused
}

// The Codex installs whose latest version the feed answers: npm's manifest
// for a launcher, the latest GitHub release for the Codex installer's.
var codexInstalls = map[string]providerupdate.Binary{
	"npm":        {Path: "/mock/codex", Launcher: "/mock/codex.js", ResolvedPath: "/mock/codex"},
	"standalone": {Path: "/mock/codex", ResolvedPath: "/home/mock/.codex/packages/standalone/releases/99.0.0-x86_64-unknown-linux-musl/codex"},
}

func newService(t *testing.T, feed *updatefake.Feed, codex providerupdate.Binary) *providerupdate.Service {
	t.Helper()
	client := &http.Client{Transport: feedTransport{feed}}
	svc := providerupdate.New(providerupdate.Deps{
		Binary: func(name string) providerupdate.Binary {
			if name == providerupdate.ProviderCodex {
				return codex
			}
			return providerupdate.Binary{Path: "/mock/" + name}
		},
		Installed:          func(string) string { return updatefake.Baseline },
		Env:                func(string) map[string]string { return nil },
		WorkDir:            func() (string, error) { return "/home/mock", nil },
		ClaudeRetention:    func(string) claude.Retention { return claude.UserHomeRetention(0) },
		ClaudeLoginExpired: func() (bool, error) { return false, nil },
		Runner: func(_ context.Context, command providerupdate.Command) (providerupdate.Output, error) {
			if slices.Equal(command.Args, []string{"doctor"}) {
				return providerupdate.Output{Text: "Auto-update channel: latest\n"}, nil
			}
			return providerupdate.Output{}, errors.New("unexpected command")
		},
		AfterRun: func() {},
		Client:   func() *http.Client { return client },
		Emit:     func([]providerupdate.Status) {},
		Now:      time.Now,
	})
	t.Cleanup(svc.Stop)
	return svc
}

func TestTheDefaultFeedOffersNoUpdate(t *testing.T) {
	t.Parallel()
	for kind, codex := range codexInstalls {
		svc := newService(t, updatefake.New(), codex)
		for _, status := range svc.Check() {
			if status.CheckError != "" || status.LatestVersion != updatefake.Baseline || status.Available {
				t.Errorf("%s (%s Codex) = %+v, want latest %s and nothing offered", status.Provider, kind, status, updatefake.Baseline)
			}
		}
	}
}

func TestASeededFeedOffersTheUpdateWithItsNotes(t *testing.T) {
	t.Parallel()
	for kind, codex := range codexInstalls {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			testASeededFeed(t, codex)
		})
	}
}

func testASeededFeed(t *testing.T, codex providerupdate.Binary) {
	feed := updatefake.New()
	svc := newService(t, feed, codex)
	if err := feed.SetLatest("99.1.0"); err != nil {
		t.Fatal(err)
	}
	for _, status := range svc.Check() {
		if status.CheckError != "" || status.LatestVersion != "99.1.0" || !status.Available {
			t.Fatalf("%s = %+v, want 99.1.0 offered", status.Provider, status)
		}
		notes, err := svc.Notes(context.Background(), status.Provider)
		if err != nil {
			t.Fatalf("%s notes: %v", status.Provider, err)
		}
		var versions []string
		for _, note := range notes.Releases {
			versions = append(versions, note.Version)
		}
		wantVersions := []string{"99.1.0", "99.0.1"}
		if status.Provider == providerupdate.ProviderAgy {
			// Antigravity serves a single Markdown changelog section for the latest release.
			wantVersions = []string{"99.1.0"}
		}
		// 99.1.0-beta.1 is listed and has a release, and is filtered out.
		if !slices.Equal(versions, wantVersions) || notes.Omitted != 0 {
			t.Errorf("%s notes = %v (omitted %d), want %v", status.Provider, versions, notes.Omitted, wantVersions)
		}
	}

	// A seed to an intermediate version publishes nothing after it.
	if err := feed.SetLatest("99.0.1"); err != nil {
		t.Fatal(err)
	}
	svc.Check()
	notes, err := svc.Notes(context.Background(), providerupdate.ProviderCodex)
	if err != nil || len(notes.Releases) != 1 || notes.Releases[0].Version != "99.0.1" {
		t.Fatalf("notes at 99.0.1 = %+v, %v", notes, err)
	}

	if !feed.Reset() || feed.Latest() != updatefake.Baseline {
		t.Fatal("Reset did not return to the baseline")
	}
	if feed.Reset() {
		t.Error("a second Reset reported a change")
	}
	for _, status := range svc.Check() {
		if status.Available {
			t.Errorf("%s still offered after Reset: %+v", status.Provider, status)
		}
	}
}

func TestSetLatestRefusesAVersionOutsideTheCatalogue(t *testing.T) {
	t.Parallel()
	feed := updatefake.New()
	for _, version := range []string{"", "99.2.0", "99.1.0-beta.1", "v99.1.0"} {
		if err := feed.SetLatest(version); err == nil {
			t.Errorf("SetLatest(%q) accepted", version)
		}
	}
	if feed.Latest() != updatefake.Baseline {
		t.Errorf("a refused seed changed the feed to %s", feed.Latest())
	}
}

func TestServeMatchesOnlyTheExactURLs(t *testing.T) {
	t.Parallel()
	feed := updatefake.New()
	for _, url := range []string{
		"https://downloads.claude.ai/claude-code-releases/nightly",
		"https://downloads.claude.ai/claude-code-releases/latest?x=1",
		"http://downloads.claude.ai/claude-code-releases/latest",
		"https://registry.npmjs.org/@openai%2fcodex/next",
		"https://api.github.com/repos/anthropics/claude-code/releases/latest",
		"https://api.github.com/repos/openai/codex/releases/latest?x=1",
		"https://api.github.com/repos/someone/else/git/matching-refs/tags/v99.",
		"https://api.github.com/repos/openai/codex/git/matching-refs/tags/rust-v99.?per_page=1",
		"https://formulae.brew.sh/api/cask/codex.json",
	} {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := feed.Serve(req); ok {
			t.Errorf("Serve answered %s", url)
		}
	}
	req, err := http.NewRequest(http.MethodPost, providerupdate.CodexNPMLatestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := feed.Serve(req); ok {
		t.Error("Serve answered a POST")
	}
	var nilFeed *updatefake.Feed
	if _, ok, _ := nilFeed.Serve(req); ok {
		t.Error("a nil feed answered")
	}
}

package providerupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const codexRepo = "openai/codex"

// codexNotesRig is installed 0.160.0, latest 0.175.0, and a tag listing
// with every shape the filter must handle.
func codexNotesRig(t *testing.T) (*rig, *atomic.Int32) {
	t.Helper()
	r := newRig(t)
	r.hosts.text(CodexNPMLatestURL, http.StatusOK, `{"version":"0.175.0"}`)
	var refs []map[string]string
	addRef := func(tag string) { refs = append(refs, map[string]string{"ref": "refs/tags/" + tag}) }
	for minor := 150; minor <= 176; minor++ {
		addRef(fmt.Sprintf("rust-v0.%d.0", minor))
		addRef(fmt.Sprintf("rust-v0.%d.0-alpha.1", minor))
	}
	addRef("rust-v0.17.0") // shares the prefix, below the range
	addRef("rust-vnext")   // not a version
	addRef("v0.170.0")     // another tag scheme
	body, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	r.hosts.text(MatchingRefsURL(codexRepo, "rust-v0."), http.StatusOK, string(body))

	var inFlight, peak atomic.Int32
	for minor := 150; minor <= 176; minor++ {
		version := fmt.Sprintf("0.%d.0", minor)
		release := map[string]any{
			"tag_name": "rust-v" + version, "name": "Codex " + version, "body": "notes " + version,
			"html_url":     "https://github.com/openai/codex/releases/tag/rust-v" + version,
			"published_at": "2026-10-01T00:00:00Z", "prerelease": minor == 171, "draft": minor == 172,
			"assets": []string{strings.Repeat("a", 1000)},
		}
		encoded, err := json.Marshal(release)
		if err != nil {
			t.Fatal(err)
		}
		url := ReleaseByTagURL(codexRepo, "rust-v"+version)
		if minor == 170 {
			r.hosts.text(url, http.StatusNotFound, `{"message":"Not Found"}`)
			continue
		}
		r.hosts.set(url, func(w http.ResponseWriter, _ *http.Request) {
			now := inFlight.Add(1)
			for {
				old := peak.Load()
				if now <= old || peak.CompareAndSwap(old, now) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			_, _ = w.Write(encoded)
		})
	}
	if _, codex := r.check(); !codex.Available {
		t.Fatalf("codex = %+v", codex)
	}
	return r, &peak
}

func TestNotesCoverTheRangeNewestFirstAndCapped(t *testing.T) {
	t.Parallel()
	r, peak := codexNotesRig(t)
	notes, err := r.svc.Notes(context.Background(), ProviderCodex)
	if err != nil {
		t.Fatalf("Notes: %v", err)
	}
	// In range: 0.161.0 through 0.175.0, fifteen stable versions. The
	// newest twelve are kept (0.164.0 up); of those 0.170.0 has no
	// release, 0.171.0 is a prerelease and 0.172.0 a draft.
	var versions []string
	for _, note := range notes.Releases {
		versions = append(versions, note.Version)
	}
	want := []string{"0.175.0", "0.174.0", "0.173.0", "0.169.0", "0.168.0", "0.167.0", "0.166.0", "0.165.0", "0.164.0"}
	if !slices.Equal(versions, want) {
		t.Fatalf("versions = %v, want %v", versions, want)
	}
	if notes.Omitted != 3 || notes.FromVersion != "0.160.0" || notes.ToVersion != "0.175.0" ||
		notes.Provider != ProviderCodex || notes.URL != "https://github.com/openai/codex/releases" {
		t.Errorf("notes = %+v", notes)
	}
	first := notes.Releases[0]
	if first.Name != "Codex 0.175.0" || first.Body != "notes 0.175.0" || first.PublishedAt != "2026-10-01T00:00:00Z" ||
		first.URL != "https://github.com/openai/codex/releases/tag/rust-v0.175.0" {
		t.Errorf("first note = %+v", first)
	}
	for _, url := range r.hosts.requested() {
		if strings.Contains(url, "alpha") || strings.HasSuffix(url, "rust-v0.163.0") || strings.HasSuffix(url, "rust-v0.176.0") ||
			strings.HasSuffix(url, "rust-v0.160.0") || strings.HasSuffix(url, "rust-v0.17.0") {
			t.Errorf("fetched %s, which is outside the kept range", url)
		}
	}
	if got := peak.Load(); got > notesFetchWorkers {
		t.Errorf("%d release fetches ran at once, want at most %d", got, notesFetchWorkers)
	}

	// Reopening reads the cache.
	before := len(r.hosts.requested())
	again, err := r.svc.Notes(context.Background(), ProviderCodex)
	if err != nil || len(again.Releases) != len(notes.Releases) {
		t.Fatalf("second Notes = %+v, %v", again, err)
	}
	if after := len(r.hosts.requested()); after != before {
		t.Errorf("the cached answer refetched %d requests", after-before)
	}

	// A new latest version is a new range.
	r.hosts.text(CodexNPMLatestURL, http.StatusOK, `{"version":"0.176.0"}`)
	r.check()
	if third, err := r.svc.Notes(context.Background(), ProviderCodex); err != nil || third.Releases[0].Version != "0.176.0" {
		t.Fatalf("Notes after a new latest = %+v, %v", third, err)
	}
}

// Two modals opening at once fetch once: the second waits and reads the
// first one's answer.
func TestConcurrentNotesLookupsFetchOnce(t *testing.T) {
	t.Parallel()
	r, _ := codexNotesRig(t)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := r.svc.Notes(context.Background(), ProviderCodex)
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := r.hosts.count(MatchingRefsURL(codexRepo, "rust-v0.")); got != 1 {
		t.Errorf("the tag listing was fetched %d times", got)
	}
}

// Stop waits for a notes lookup in flight, even one whose request ignores
// cancellation, and the lookup waiting its turn behind it is refused rather
// than fetching after Stop.
func TestStopWaitsForNotesLookups(t *testing.T) {
	t.Parallel()
	r, _ := codexNotesRig(t)
	listing := MatchingRefsURL(codexRepo, "rust-v0.")
	reached, release := r.hosts.hold(t, listing)
	errs := make(chan error, 2)
	notes := func() {
		_, err := r.svc.Notes(context.Background(), ProviderCodex)
		errs <- err
	}
	go notes()
	<-reached
	go notes()
	// Let the second lookup reach the first one's lock.
	time.Sleep(50 * time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		r.svc.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a notes lookup was still fetching")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return once the lookup ended")
	}
	var shuttingDown int
	for range 2 {
		err := <-errs
		if err == nil {
			t.Fatal("a notes lookup succeeded across Stop")
		}
		if errors.Is(err, ErrShuttingDown) {
			shuttingDown++
		}
	}
	if shuttingDown != 1 {
		t.Errorf("%d lookups refused for shutdown, want the one waiting its turn", shuttingDown)
	}
	if got := r.hosts.count(listing); got > 1 {
		t.Errorf("the tag listing was requested %d times; the waiting lookup fetched after Stop", got)
	}
}

// After Stop, notes are refused even when the answer is cached.
func TestNotesAfterStopAreRefused(t *testing.T) {
	t.Parallel()
	r, _ := codexNotesRig(t)
	if _, err := r.svc.Notes(context.Background(), ProviderCodex); err != nil {
		t.Fatal(err)
	}
	r.svc.Stop()
	before := len(r.hosts.requested())
	if _, err := r.svc.Notes(context.Background(), ProviderCodex); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Notes after Stop = %v, want ErrShuttingDown", err)
	}
	if after := len(r.hosts.requested()); after != before {
		t.Errorf("Notes after Stop made %d requests", after-before)
	}
}

// A range across a change in digit count keeps every version in it: the
// listing is narrowed by whole shared components, not shared characters.
func TestNotesKeepEveryVersionAcrossADigitCountChange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider, repo, tagPrefix, installed, latest, listing string
		tags                                                  func() []string
		want                                                  []string
		omitted                                               int
	}{
		{
			provider: ProviderClaude, repo: "anthropics/claude-code", tagPrefix: "v",
			installed: "2.1.3", latest: "2.1.30", listing: "v2.1.",
			tags: func() (tags []string) {
				for patch := 0; patch <= 35; patch++ {
					tags = append(tags, fmt.Sprintf("2.1.%d", patch))
				}
				return tags
			},
			// 2.1.4 through 2.1.30 are in range: the newest 12 kept.
			want:    versionRange("2.1.%d", 30, 19),
			omitted: 15,
		},
		{
			provider: ProviderCodex, repo: codexRepo, tagPrefix: "rust-v",
			installed: "0.16.0", latest: "0.169.0", listing: "rust-v0.",
			tags: func() (tags []string) {
				for minor := 10; minor <= 170; minor++ {
					tags = append(tags, fmt.Sprintf("0.%d.0", minor))
				}
				return tags
			},
			// 0.17.0 through 0.169.0 are in range.
			want:    versionRange("0.%d.0", 169, 158),
			omitted: 141,
		},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.setInstalled(tc.provider, tc.installed)
			r.hosts.text(ClaudeChannelURL("latest"), http.StatusOK, tc.latest)
			r.hosts.text(CodexNPMLatestURL, http.StatusOK, `{"version":"`+tc.latest+`"}`)
			// The listing answers every prefix as GitHub does, with the tags
			// that start with it, so a narrower listing loses versions
			// rather than missing a route.
			tags := tc.tags()
			for _, version := range tags {
				r.hosts.text(ReleaseByTagURL(tc.repo, tc.tagPrefix+version), http.StatusOK, `{"body":"`+version+`"}`)
				tag := tc.tagPrefix + version
				for end := 0; end <= len(tag); end++ {
					var refs []string
					for _, other := range tags {
						if strings.HasPrefix(tc.tagPrefix+other, tag[:end]) {
							refs = append(refs, `{"ref":"refs/tags/`+tc.tagPrefix+other+`"}`)
						}
					}
					r.hosts.text(MatchingRefsURL(tc.repo, tag[:end]), http.StatusOK, "["+strings.Join(refs, ",")+"]")
				}
			}
			r.check()
			if got := statusOf(r.svc.Snapshot(), tc.provider); !got.Available {
				t.Fatalf("status = %+v", got)
			}
			notes, err := r.svc.Notes(context.Background(), tc.provider)
			if err != nil {
				t.Fatalf("Notes: %v", err)
			}
			var got []string
			for _, note := range notes.Releases {
				got = append(got, note.Version)
			}
			if !slices.Equal(got, tc.want) || notes.Omitted != tc.omitted {
				t.Fatalf("versions = %v (omitted %d), want %v (omitted %d)", got, notes.Omitted, tc.want, tc.omitted)
			}
			if r.hosts.count(MatchingRefsURL(tc.repo, tc.listing)) != 1 {
				t.Errorf("requests = %v, want the %s listing", r.hosts.requested(), tc.listing)
			}
		})
	}
}

// versionRange formats the numbers from newest down to oldest.
func versionRange(format string, newest, oldest int) []string {
	var out []string
	for n := newest; n >= oldest; n-- {
		out = append(out, fmt.Sprintf(format, n))
	}
	return out
}

func TestAFailedNotesFetchIsNotCached(t *testing.T) {
	t.Parallel()
	r, _ := codexNotesRig(t)
	failing := ReleaseByTagURL(codexRepo, "rust-v0.168.0")
	r.hosts.text(failing, http.StatusForbidden, `{"message":"API rate limit exceeded"}`)
	if _, err := r.svc.Notes(context.Background(), ProviderCodex); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Notes = %v, want the 403 reported", err)
	}
	r.hosts.text(failing, http.StatusOK, `{"name":"Codex 0.168.0","body":"back"}`)
	notes, err := r.svc.Notes(context.Background(), ProviderCodex)
	if err != nil {
		t.Fatalf("Notes after recovery: %v", err)
	}
	if !slices.ContainsFunc(notes.Releases, func(n Note) bool { return n.Body == "back" }) {
		t.Errorf("the recovered fetch did not run: %+v", notes.Releases)
	}
}

func TestNotesNeedAKnownUpdate(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := r.svc.Notes(context.Background(), ProviderClaude); err == nil {
		t.Error("Notes answered before any check")
	}
	r.setInstalled(ProviderClaude, "2.1.325")
	r.check()
	if _, err := r.svc.Notes(context.Background(), ProviderClaude); err == nil || !strings.Contains(err.Error(), "no claude update") {
		t.Errorf("Notes with no update = %v", err)
	}
	if _, err := r.svc.Notes(context.Background(), "gemini"); err == nil {
		t.Error("Notes answered an unknown provider")
	}
	if len(r.hosts.requested()) != 2 { // the channel and the npm manifest
		t.Errorf("requests = %v", r.hosts.requested())
	}
}

func TestClaudeNotesUseItsRepoAndPrefix(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.setInstalled(ProviderClaude, "2.1.321")
	r.check()
	r.hosts.text(MatchingRefsURL("anthropics/claude-code", "v2.1."), http.StatusOK,
		`[{"ref":"refs/tags/v2.1.320"},{"ref":"refs/tags/v2.1.321"},{"ref":"refs/tags/v2.1.323"},{"ref":"refs/tags/v2.1.325"},{"ref":"refs/tags/v2.1.326"}]`)
	r.hosts.text(ReleaseByTagURL("anthropics/claude-code", "v2.1.323"), http.StatusOK, `{"name":"v2.1.323","body":"b323"}`)
	r.hosts.text(ReleaseByTagURL("anthropics/claude-code", "v2.1.325"), http.StatusOK, `{"name":"v2.1.325","body":"b325"}`)
	notes, err := r.svc.Notes(context.Background(), ProviderClaude)
	if err != nil {
		t.Fatalf("Notes: %v", err)
	}
	if len(notes.Releases) != 2 || notes.Releases[0].Version != "2.1.325" || notes.Releases[1].Body != "b323" ||
		notes.URL != "https://github.com/anthropics/claude-code/releases" || notes.Omitted != 0 {
		t.Fatalf("notes = %+v", notes)
	}
}

func TestNotesRefusesAnOversizedRelease(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.setInstalled(ProviderClaude, "2.1.321")
	r.check()
	r.hosts.text(MatchingRefsURL("anthropics/claude-code", "v2.1."), http.StatusOK, `[{"ref":"refs/tags/v2.1.325"}]`)
	r.hosts.text(ReleaseByTagURL("anthropics/claude-code", "v2.1.325"), http.StatusOK,
		`{"body":"`+strings.Repeat("x", releaseBodyLimit)+`"}`)
	if _, err := r.svc.Notes(context.Background(), ProviderClaude); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("Notes = %v, want the oversized release refused", err)
	}
}

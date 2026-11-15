package providerupdate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

// Notes bounds. A Codex range can span hundreds of tags, and each release
// is fetched on its own.
const (
	notesMaxReleases  = 12
	notesFetchWorkers = 4
	NotesTimeout      = 60 * time.Second
)

// releaseRepo is where a provider publishes its release notes on GitHub.
type releaseRepo struct {
	repo      string
	tagPrefix string
}

var releaseRepos = map[string]releaseRepo{
	ProviderClaude: {repo: "anthropics/claude-code", tagPrefix: "v"},
	ProviderCodex:  {repo: "openai/codex", tagPrefix: "rust-v"},
}

// MatchingRefsURL lists every tag of repo that starts with prefix, in one
// response.
func MatchingRefsURL(repo, prefix string) string {
	return githubAPIBase + repo + "/git/matching-refs/tags/" + prefix
}

// ReleaseByTagURL is one release of repo.
func ReleaseByTagURL(repo, tag string) string {
	return githubAPIBase + repo + "/releases/tags/" + tag
}

// ReleasesPageURL is repo's human-readable releases page.
func ReleasesPageURL(repo string) string { return "https://github.com/" + repo + "/releases" }

// notesCache is the last successful answer per provider, valid while the
// installed and latest versions it was fetched for are the current ones.
type notesCache struct {
	installed, latest string
	notes             Notes
}

// Notes returns the release notes of every stable version after the
// installed one up to the latest, newest first. It needs a check to have
// found an update. A provider's notes lookups run one at a time, so a
// reopened modal reads the cached answer instead of fetching again. Once
// Stop has run it is refused with ErrShuttingDown, cached or not, and Stop
// waits for every lookup it admitted, including one waiting its turn.
func (s *Service) Notes(ctx context.Context, providerName string) (Notes, error) {
	providerState, ok := s.providers[providerName]
	if !ok {
		return Notes{}, unknownProvider(providerName)
	}
	if err := s.admit(); err != nil {
		return Notes{}, fmt.Errorf("cannot read the %s release notes: %w", providerName, err)
	}
	defer s.work.Done()
	providerState.notesMu.Lock()
	defer providerState.notesMu.Unlock()
	if s.life.Err() != nil {
		return Notes{}, fmt.Errorf("cannot read the %s release notes: %w", providerName, ErrShuttingDown)
	}

	s.mu.Lock()
	status := providerState.status
	cached := providerState.notes
	s.mu.Unlock()
	if status.InstalledVersion == "" || status.LatestVersion == "" || !status.Available {
		return Notes{}, fmt.Errorf("no %s update is known, so there are no release notes to show", providerName)
	}
	if cached != nil && cached.installed == status.InstalledVersion && cached.latest == status.LatestVersion {
		return cached.notes, nil
	}

	ctx, cancel := context.WithTimeout(ctx, NotesTimeout)
	defer cancel()
	stop := context.AfterFunc(s.life, cancel)
	defer stop()
	notes, err := s.fetchNotes(ctx, providerName, status.InstalledVersion, status.LatestVersion)
	if err != nil {
		return Notes{}, fmt.Errorf("could not read the %s release notes: %w", providerName, err)
	}
	s.mu.Lock()
	providerState.notes = &notesCache{installed: status.InstalledVersion, latest: status.LatestVersion, notes: notes}
	s.mu.Unlock()
	return notes, nil
}

func (s *Service) fetchNotes(ctx context.Context, providerName, installed, latest string) (Notes, error) {
	repo := releaseRepos[providerName]
	from, to := canonical(installed), canonical(latest)
	if from == "" {
		return Notes{}, errUnparseable(installed)
	}
	if to == "" {
		return Notes{}, errUnparseable(latest)
	}
	var refs []struct {
		Ref string `json:"ref"`
	}
	refsURL := MatchingRefsURL(repo.repo, repo.tagPrefix+sharedComponents(installed, latest))
	if err := s.getJSON(ctx, refsURL, refsBodyLimit, &refs); err != nil {
		return Notes{}, err
	}

	type candidate struct {
		token string
		v     string // canonical
	}
	var inRange []candidate
	for _, ref := range refs {
		token, ok := strings.CutPrefix(ref.Ref, "refs/tags/"+repo.tagPrefix)
		if !ok {
			continue
		}
		v := canonical(token)
		if v == "" || semver.Prerelease(v) != "" || semver.Compare(v, from) <= 0 || semver.Compare(v, to) > 0 {
			continue
		}
		inRange = append(inRange, candidate{token: token, v: v})
	}
	slices.SortFunc(inRange, func(a, b candidate) int { return semver.Compare(b.v, a.v) })
	omitted := max(0, len(inRange)-notesMaxReleases)
	inRange = inRange[:len(inRange)-omitted]

	found := make([]*Note, len(inRange))
	errs := make([]error, len(inRange))
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(notesFetchWorkers, len(inRange)) {
		wg.Go(func() {
			for i := range work {
				found[i], errs[i] = s.fetchRelease(ctx, repo, inRange[i].token)
			}
		})
	}
	for i := range inRange {
		work <- i
	}
	close(work)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return Notes{}, err
	}

	notes := Notes{
		Provider:    providerName,
		FromVersion: installed,
		ToVersion:   latest,
		Releases:    make([]Note, 0, len(found)),
		Omitted:     omitted,
		URL:         ReleasesPageURL(repo.repo),
	}
	for _, note := range found {
		if note != nil {
			notes.Releases = append(notes.Releases, *note)
		}
	}
	return notes, nil
}

// fetchRelease returns one tag's published release, nil when the tag has
// no release or its release is a draft or prerelease.
func (s *Service) fetchRelease(ctx context.Context, repo releaseRepo, token string) (*Note, error) {
	var release struct {
		Name        string `json:"name"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Prerelease  bool   `json:"prerelease"`
		Draft       bool   `json:"draft"`
	}
	err := s.getJSON(ctx, ReleaseByTagURL(repo.repo, repo.tagPrefix+token), releaseBodyLimit, &release)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if release.Draft || release.Prerelease {
		return nil, nil
	}
	return &Note{
		Version:     token,
		Name:        release.Name,
		PublishedAt: release.PublishedAt,
		Body:        release.Body,
		URL:         release.HTMLURL,
	}, nil
}

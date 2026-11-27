package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
)

// The forge pull request history a ticket's Work lists
// (docs/architecture/tracker-backend.md#forge-pull-request-history): the
// app keeps one source per forge repository a project of an enabled
// tracker connection clones, and one per parent of such a repository that
// is a fork, and reads each one in the linked loop's rounds
// (trackerapp Deps.ReadForgeHistory): every forgeHistoryEvery, or at the
// next round while its walk has work left, at most forgeHistoryConcurrency
// at once, and in SearchTicketWork when its last read is older than
// forgeHistoryCatchUp. One read is at most gitops.PRHistoryRequests
// requests.

const (
	// forgeHistoryEvery is how long a source whose walk has no work left
	// rests between the loop's reads of it.
	forgeHistoryEvery = 15 * time.Minute
	// forgeHistoryCatchUp is how old a source's last read is before
	// SearchTicketWork reads it again.
	forgeHistoryCatchUp = time.Minute
	// forgeHistoryConcurrency bounds the sources one round reads at once.
	forgeHistoryConcurrency = 4
)

// forgeHistoryState serializes the source set's writes and each source's
// reads.
type forgeHistoryState struct {
	// setMu holds a computation of the wanted sources through its write,
	// so a computation that read the connections before a change never
	// writes after one that read them after it.
	setMu sync.Mutex
	mu    sync.Mutex
	// reading holds a one-slot lock per source id, held across a read and
	// its write so two reads of one source never run at once, and taken
	// within the caller's context. It keeps one entry per source this
	// process has read.
	reading map[int64]chan struct{}
	// now replaces the wall clock in tests.
	now func() time.Time
}

func (a *App) forgeHistoryNow() time.Time {
	if a.forgeHistory.now != nil {
		return a.forgeHistory.now()
	}
	return time.Now()
}

// forgeHistoryWindowStart is the start of the window the history keeps,
// the tracker mirror's (trackerapp.HistoryWindow).
func forgeHistoryWindowStart(now time.Time) time.Time { return now.Add(-trackerapp.HistoryWindow) }

// forgeHistoryRef is the source of repo.
func forgeHistoryRef(repo gitops.RepoReference) store.ForgeHistoryRef {
	return store.ForgeHistoryRef{RepoKey: repo.Identity(), Forge: repo.Forge, Host: repo.Host, Namespace: repo.Namespace, Repo: repo.Repo}
}

// forgeHistoryRepo is the repository a source reads.
func forgeHistoryRepo(ref store.ForgeHistoryRef) gitops.RepoReference {
	return gitops.RepoReference{Forge: ref.Forge, Host: ref.Host, Namespace: ref.Namespace, Repo: ref.Repo}
}

// forgeHistoryDue is when a round reads a source: never read, its walk
// has work left, or its last read is forgeHistoryEvery old.
func forgeHistoryDue(src store.ForgeHistorySource, now time.Time) bool {
	return src.ReadAt == 0 || src.Owed || now.Sub(time.UnixMilli(src.ReadAt)) >= forgeHistoryEvery
}

// forgeHistoryRoundDue is the round's rule for reading a source: a client
// is connected, judged once the source's lock is held, and the source is
// due (forgeHistoryDue).
func (a *App) forgeHistoryRoundDue(src store.ForgeHistorySource, now time.Time) bool {
	return a.clientConnected() && forgeHistoryDue(src, now)
}

// forgeHistoryStale is SearchTicketWork's rule for reading a source: never
// read, or its last read is forgeHistoryCatchUp old.
func forgeHistoryStale(src store.ForgeHistorySource, now time.Time) bool {
	return src.ReadAt == 0 || now.Sub(time.UnixMilli(src.ReadAt)) >= forgeHistoryCatchUp
}

// clientConnected reports whether a client is connected: the linked loop
// and the forge history's rounds do their work only then.
func (a *App) clientConnected() bool {
	bus := a.eventBus.Load()
	return bus != nil && bus.SubscriberCount() > 0
}

// readForgeHistory is the linked loop's round of the forge history
// (trackerapp Deps.ReadForgeHistory): it makes the stored sources the
// wanted ones (syncForgeHistorySources) and reads each one due
// (forgeHistoryRoundDue) once, at most forgeHistoryConcurrency at once,
// at background priority. A source's forge failure is recorded on it, where Work shows
// it; the error returned is the rest, joined.
func (a *App) readForgeHistory(ctx context.Context) error {
	sources, unread, err := a.syncForgeHistorySources(ctx)
	if err != nil {
		return err
	}
	now := a.forgeHistoryNow()
	sources = slices.DeleteFunc(sources, func(s store.ForgeHistorySource) bool { return !forgeHistoryDue(s, now) })
	errs := make([]error, len(sources))
	runBounded(len(sources), forgeHistoryConcurrency, func(i int) {
		errs[i] = a.readForgeHistorySource(ctx, sources[i].ID, a.forgeHistoryRoundDue)
	})
	if ctx.Err() != nil {
		return nil
	}
	return errors.Join(append(errs, unread)...)
}

// syncForgeHistorySources makes the stored sources the wanted ones and
// answers them: the origin of every project, not archived, that lists a
// board of an enabled Jira or Linear connection, and the parent of each
// such origin that a landed read found to be a fork; a parent's own
// parent is not wanted for it. A source is one
// repository however many projects want it, as an origin or as a parent,
// so a change of who wants it removes nothing it holds. When git cannot
// read a project's origin, no stored source is removed, since that origin
// may be one of them: the wanted ones are added, and unread says which
// projects' origins git could not read.
func (a *App) syncForgeHistorySources(ctx context.Context) (_ []store.ForgeHistorySource, unread, _ error) {
	a.forgeHistory.setMu.Lock()
	defer a.forgeHistory.setMu.Unlock()
	svc, err := a.trackerService()
	if err != nil {
		return nil, nil, err
	}
	choices, err := svc.BoardChoices()
	if err != nil {
		return nil, nil, fmt.Errorf("read the projects whose pull request history the app keeps: %w", err)
	}
	var projects []string
	for _, c := range choices {
		if c.ConnectionEnabled && !c.ProjectArchived && c.ConnectionKind != store.TrackerKindTempo && !slices.Contains(projects, c.Project) {
			projects = append(projects, c.Project)
		}
	}
	var wanted []store.ForgeHistoryRef
	want := func(ref store.ForgeHistoryRef) {
		if !slices.ContainsFunc(wanted, func(r store.ForgeHistoryRef) bool { return r.RepoKey == ref.RepoKey }) {
			wanted = append(wanted, ref)
		}
	}
	var origins []error
	for _, id := range projects {
		project, err := a.store.GetProject(id)
		if err != nil {
			return nil, nil, fmt.Errorf("read project %s for its pull request history: %w", id, err)
		}
		origin, ok, err := a.forgeProject(ctx, project)
		switch {
		case err != nil:
			return nil, nil, err
		case !ok:
		case origin.Error != "":
			origins = append(origins, fmt.Errorf("project %s: %s", project.Name, origin.Error))
		default:
			want(forgeHistoryRef(origin.Repo))
		}
	}
	stored, err := a.store.ListForgeHistorySources()
	if err != nil {
		return nil, nil, err
	}
	originRefs := slices.Clone(wanted)
	for _, s := range stored {
		if s.Parent.RepoKey != "" && slices.ContainsFunc(originRefs, func(r store.ForgeHistoryRef) bool { return r.RepoKey == s.RepoKey }) {
			want(s.Parent)
		}
	}
	if len(origins) > 0 {
		unread = fmt.Errorf("the pull request history keeps every repository it reads, since git could not read the origin of: %w", errors.Join(origins...))
		for _, s := range stored {
			want(s.ForgeHistoryRef)
		}
	}
	changed, err := a.store.SetForgeHistorySources(wanted)
	if err != nil {
		return nil, nil, err
	}
	if !changed {
		return stored, unread, nil
	}
	stored, err = a.store.ListForgeHistorySources()
	if err != nil {
		return nil, nil, err
	}
	return stored, unread, nil
}

// followForgeHistorySources makes the stored sources follow a change of
// the connections at once, so turning off or removing a connection drops
// the history kept for it without waiting for a round, which may never
// come when no connection is left on. Its failure is the change's: the
// change stands, and the history follows at the next change or round. A
// project whose origin git could not read is logged, as the loop's
// rounds report it: it is not the change's failure.
func (a *App) followForgeHistorySources(ctx context.Context) error {
	_, unread, err := a.syncForgeHistorySources(ctx)
	if err != nil {
		return fmt.Errorf("the connection changed, but the forge pull request history kept for it could not follow: %w", err)
	}
	if unread != nil {
		log.Printf("forge history: %v", unread)
	}
	return nil
}

// lockForgeHistorySource takes source id's read lock within ctx and
// answers its release.
func (a *App) lockForgeHistorySource(ctx context.Context, id int64) (func(), error) {
	a.forgeHistory.mu.Lock()
	if a.forgeHistory.reading == nil {
		a.forgeHistory.reading = map[int64]chan struct{}{}
	}
	lock := a.forgeHistory.reading[id]
	if lock == nil {
		lock = make(chan struct{}, 1)
		a.forgeHistory.reading[id] = lock
	}
	a.forgeHistory.mu.Unlock()
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// forgeHistoryFailureDetail is what a source's FailureDetail holds: the
// parts of a failed read's failure (newRepoForgeFailure) a Pull requests
// page project carries beside its sentence and kind.
type forgeHistoryFailureDetail struct {
	ErrorDetails *ForgeErrorDetail `json:"errorDetails,omitempty"`
	Reserve      bool              `json:"reserve,omitempty"`
	ResumeAt     string            `json:"resumeAt,omitempty"`
}

// readForgeHistorySource reads the next part of source id's history, one
// read of at most gitops.PRHistoryRequests requests, and stores it, when
// due says the source is due. It waits for another read of the source
// within ctx, and then judges due on what that read left, so a caller
// that wanted a fresh read joins one that just landed. A read ctx ends
// stores nothing. A forge failure is recorded on the source with what the
// read read before it, and logged under its correlation id; the error
// returned is the store's, or ctx's. A source removed meanwhile, or
// before the write, is no error: the write lands only in the source the
// read began from.
func (a *App) readForgeHistorySource(ctx context.Context, id int64, due func(store.ForgeHistorySource, time.Time) bool) error {
	release, err := a.lockForgeHistorySource(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	src, ok, err := a.store.GetForgeHistorySource(id)
	if err != nil || !ok {
		return err
	}
	now := a.forgeHistoryNow()
	if !due(src, now) {
		return nil
	}
	repo := forgeHistoryRepo(src.ForgeHistoryRef)
	windowStart := forgeHistoryWindowStart(now)
	h, readErr := a.gitCore().ReadPRHistory(ctx, repo, gitops.PRHistoryInput{Walk: src.Walk, WindowStart: windowStart,
		Stored: func(numbers []int64) (map[int64]gitops.StoredPR, error) {
			return forgeHistoryStored(id, a.store, numbers)
		}})
	if err := ctx.Err(); err != nil {
		return err
	}
	read := forgeHistoryRead(src, h)
	read.ReadAt, read.WindowStart = now.UnixMilli(), windowStart.UnixMilli()
	if readErr != nil {
		fail := a.newRepoForgeFailure(repo, readErr)
		log.Printf("forge history: read of %s failed (id: %s): %v", repo.Key(), fail.correlationID, readErr)
		detail, err := json.Marshal(forgeHistoryFailureDetail{ErrorDetails: fail.detail, Reserve: fail.reserve, ResumeAt: fail.resumeAt})
		if err != nil {
			return fmt.Errorf("record the failed read of %s: %w", repo.Key(), err)
		}
		read.FailureKind, read.Failure, read.FailureDetail = fail.kind, fail.wire, detail
	}
	if err := a.store.ApplyForgeHistory(read); err != nil && !errors.Is(err, store.ErrForgeHistoryGone) {
		return err
	}
	return nil
}

// forgeHistoryStored is what source id stores of numbers, as a reader
// takes it.
func forgeHistoryStored(id int64, s *store.Store, numbers []int64) (map[int64]gitops.StoredPR, error) {
	stored, err := s.StoredForgeHistoryPRs(id, numbers)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]gitops.StoredPR, len(stored))
	for n, pr := range stored {
		out[n] = gitops.StoredPR{Updated: time.UnixMilli(pr.UpdatedAt), Seen: time.UnixMilli(pr.SeenAt)}
	}
	return out, nil
}

// forgeHistoryRead is h as the store applies it to src.
func forgeHistoryRead(src store.ForgeHistorySource, h gitops.PRHistory) store.ForgeHistoryRead {
	read := store.ForgeHistoryRead{SourceID: src.ID, Source: src.ForgeHistoryRef, OwnHead: h.OwnHead, Walk: h.Walk,
		Reach: forgeHistoryMilli(h.Reach), HasReach: h.HasReach, Owed: h.Owed}
	if h.Parent != nil {
		read.Parent = forgeHistoryRef(*h.Parent)
	}
	for _, pr := range h.PRs {
		read.PRs = append(read.PRs, store.ForgeHistoryPR{
			Number: pr.Number, URL: pr.URL, Title: pr.Title, HeadRef: pr.HeadRef, HeadRepo: pr.HeadRepo, State: pr.State,
			Author: pr.Author.Login, AuthorName: pr.Author.Name, CreatedAt: forgeHistoryMilli(pr.Created), UpdatedAt: forgeHistoryMilli(pr.Updated),
			MergedAt: forgeHistoryMilli(pr.Merged), ClosedAt: forgeHistoryMilli(pr.Closed), SeenAt: forgeHistoryMilli(pr.Seen),
			Keys: trackerapp.TicketKeys(pr.Title + " " + pr.HeadRef),
		})
	}
	read.Acts = forgeHistoryActs(h.Acts)
	return read
}

// forgeHistoryActs are acts as the store keeps them; a person the forge
// named only by login has no name, and the store keeps one it has.
func forgeHistoryActs(acts []gitops.PRHistoryAct) []store.ForgeHistoryAct {
	out := make([]store.ForgeHistoryAct, 0, len(acts))
	for _, act := range acts {
		kind := store.ForgeActCommented
		if act.Approved {
			kind = store.ForgeActApproved
		}
		out = append(out, store.ForgeHistoryAct{Number: act.Number, Kind: kind, Login: act.Person.Login, Name: act.Person.Name, At: forgeHistoryMilli(act.At)})
	}
	return out
}

// forgeHistoryMilli is t in Unix ms, 0 for the zero time.
func forgeHistoryMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

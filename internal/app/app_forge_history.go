package app

import (
	"cmp"
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
// app keeps one source per repository a project of an enabled tracker
// connection clones, and one per fork's parent for the pull requests
// whose head is the fork, and reads each one in the linked loop's rounds
// (trackerapp Deps.ReadForgeHistory) every forgeHistoryEvery, at most
// forgeHistoryConcurrency at once, and in SearchTicketWork when its last
// read is older than forgeHistoryCatchUp.

const (
	// forgeHistoryEvery is how long a source rests between the loop's
	// reads of it.
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
	// reading holds a lock per source, by forgeHistoryLockKey, held
	// across a read and its write so two reads of one source never run
	// at once. It keeps one entry per source this process has read.
	reading map[string]*sync.Mutex
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

// forgeHistoryRef is the source of origin repo, whose every pull request
// is kept.
func forgeHistoryRef(repo gitops.RepoReference) store.ForgeHistoryRef {
	return store.ForgeHistoryRef{RepoKey: repo.Identity(), Forge: repo.Forge, Host: repo.Host, Namespace: repo.Namespace, Repo: repo.Repo}
}

// forgeHistoryRepo is the repository a source reads.
func forgeHistoryRepo(ref store.ForgeHistoryRef) gitops.RepoReference {
	return gitops.RepoReference{Forge: ref.Forge, Host: ref.Host, Namespace: ref.Namespace, Repo: ref.Repo}
}

// forgeHistoryParent is the source that reads the parent of fork, a
// stored origin source, for the pull requests whose head is fork; false
// when the forge names no parent of it, or no read of it has landed.
func forgeHistoryParent(fork store.ForgeHistorySource) (store.ForgeHistoryRef, bool) {
	if fork.KeepHead != "" || fork.OwnHead == "" || fork.ParentRepo == "" {
		return store.ForgeHistoryRef{}, false
	}
	parent := gitops.RepoReference{Forge: fork.Forge, Host: fork.Host, Namespace: fork.ParentNamespace, Repo: fork.ParentRepo}
	ref := forgeHistoryRef(parent)
	ref.KeepHead = fork.OwnHead
	return ref, true
}

func forgeHistoryLockKey(ref store.ForgeHistoryRef) string {
	return ref.RepoKey + "\x00" + ref.KeepHead
}

// readForgeHistory is the linked loop's round of the forge history
// (trackerapp Deps.ReadForgeHistory): it makes the stored sources the
// wanted ones (syncForgeHistorySources) and reads each one whose last
// read is forgeHistoryEvery old, at most forgeHistoryConcurrency at once,
// at background priority. A source's forge failure is recorded on it,
// where Work shows it; the error returned is the rest, joined.
func (a *App) readForgeHistory(ctx context.Context) error {
	sources, unread, err := a.syncForgeHistorySources(ctx)
	if err != nil {
		return err
	}
	errs := make([]error, len(sources))
	runBounded(len(sources), forgeHistoryConcurrency, func(i int) {
		errs[i] = a.readForgeHistorySource(ctx, sources[i].ForgeHistoryRef, forgeHistoryEvery)
	})
	if ctx.Err() != nil {
		return nil
	}
	return errors.Join(append(errs, unread)...)
}

// syncForgeHistorySources makes the stored sources the wanted ones and
// answers them: the origin of every project, not archived, that lists a
// board of an enabled Jira or Linear connection, and the parent of each
// such origin that a landed read found to be a fork, unless the parent
// is itself one of the origins, which keeps every pull request. When git
// cannot read a project's origin, no stored source is removed, since that
// origin may be one of them: the wanted ones are added, and unread says
// which projects' origins git could not read.
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
		case !slices.ContainsFunc(wanted, func(r store.ForgeHistoryRef) bool { return r.RepoKey == origin.Repo.Identity() }):
			wanted = append(wanted, forgeHistoryRef(origin.Repo))
		}
	}
	stored, err := a.store.ListForgeHistorySources()
	if err != nil {
		return nil, nil, err
	}
	read := len(wanted)
	for _, s := range stored {
		parent, ok := forgeHistoryParent(s)
		if ok && slices.ContainsFunc(wanted[:read], func(r store.ForgeHistoryRef) bool { return r.RepoKey == s.RepoKey }) &&
			!slices.ContainsFunc(wanted[:read], func(r store.ForgeHistoryRef) bool { return r.RepoKey == parent.RepoKey }) {
			wanted = append(wanted, parent)
		}
	}
	if len(origins) > 0 {
		unread = fmt.Errorf("the pull request history keeps every repository it reads, since git could not read the origin of: %w", errors.Join(origins...))
		for _, s := range stored {
			if !slices.ContainsFunc(wanted, func(r store.ForgeHistoryRef) bool {
				return forgeHistoryLockKey(r) == forgeHistoryLockKey(s.ForgeHistoryRef)
			}) {
				wanted = append(wanted, s.ForgeHistoryRef)
			}
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

// forgeHistorySource is the stored source ref; false when it is not
// stored.
func (a *App) forgeHistorySource(ref store.ForgeHistoryRef) (store.ForgeHistorySource, bool, error) {
	all, err := a.store.ListForgeHistorySources()
	if err != nil {
		return store.ForgeHistorySource{}, false, err
	}
	at := slices.IndexFunc(all, func(s store.ForgeHistorySource) bool {
		return forgeHistoryLockKey(s.ForgeHistoryRef) == forgeHistoryLockKey(ref)
	})
	if at < 0 {
		return store.ForgeHistorySource{}, false, nil
	}
	return all[at], true, nil
}

// forgeHistoryFailureDetail is what a source's FailureDetail holds: the
// parts of a failed read's failure (newRepoForgeFailure) a Pull requests
// page project carries beside its sentence and kind.
type forgeHistoryFailureDetail struct {
	ErrorDetails *ForgeErrorDetail `json:"errorDetails,omitempty"`
	Reserve      bool              `json:"reserve,omitempty"`
	ResumeAt     string            `json:"resumeAt,omitempty"`
}

// readForgeHistorySource reads the next part of ref's history and stores
// it, unless its last read is younger than olderThan, or another read of
// it is running, after which it reads only if that read left it so old.
// A read left behind the newest updates (git.PRHistory.Behind) is stored
// and followed at once by the next, until one is not, so a repository
// that changes faster than one read holds is never left behind for a
// round. A read ctx ends stores nothing. A forge failure is recorded on
// the source with what the read read before it, and logged under its
// correlation id; the error returned is the store's, or ctx's. A source
// removed meanwhile is no error.
func (a *App) readForgeHistorySource(ctx context.Context, ref store.ForgeHistoryRef, olderThan time.Duration) error {
	a.forgeHistory.mu.Lock()
	if a.forgeHistory.reading == nil {
		a.forgeHistory.reading = map[string]*sync.Mutex{}
	}
	lock := a.forgeHistory.reading[forgeHistoryLockKey(ref)]
	if lock == nil {
		lock = &sync.Mutex{}
		a.forgeHistory.reading[forgeHistoryLockKey(ref)] = lock
	}
	a.forgeHistory.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	src, ok, err := a.forgeHistorySource(ref)
	if err != nil || !ok {
		return err
	}
	now := a.forgeHistoryNow()
	if src.ReadAt > 0 && now.Sub(time.UnixMilli(src.ReadAt)) < olderThan {
		return nil
	}
	repo := forgeHistoryRepo(ref)
	walk := src.Walk
	for {
		windowStart := forgeHistoryWindowStart(now)
		h, readErr := a.gitCore().ReadPRHistory(ctx, repo, gitops.PRHistoryInput{Walk: walk, WindowStart: windowStart,
			Known: func(numbers []int64) (map[int64]bool, error) { return a.store.KnownForgeHistoryPRs(ref, numbers) }})
		if err := ctx.Err(); err != nil {
			return err
		}
		read := forgeHistoryRead(ref, h)
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
		err = a.store.ApplyForgeHistory(read)
		switch {
		case errors.Is(err, store.ErrForgeHistoryGone):
			return nil
		case err != nil || readErr != nil || !h.Behind:
			return err
		}
		walk, now = h.Walk, a.forgeHistoryNow()
	}
}

// forgeHistoryRead is h as the store applies it to ref.
func forgeHistoryRead(ref store.ForgeHistoryRef, h gitops.PRHistory) store.ForgeHistoryRead {
	read := store.ForgeHistoryRead{Source: ref, OwnHead: h.OwnHead, Walk: h.Walk, Reach: forgeHistoryMilli(h.Reach), Walked: h.Walked, Behind: h.Behind}
	if h.Parent != nil {
		read.ParentNamespace, read.ParentRepo = h.Parent.Namespace, h.Parent.Repo
	}
	for _, pr := range h.PRs {
		row := store.ForgeHistoryPR{
			Number: pr.Number, URL: pr.URL, Title: pr.Title, HeadRef: pr.HeadRef, HeadRepo: pr.HeadRepo, State: pr.State,
			Author: pr.Author.Login, AuthorName: pr.Author.Name, CreatedAt: forgeHistoryMilli(pr.Created), UpdatedAt: forgeHistoryMilli(pr.Updated),
			MergedAt: forgeHistoryMilli(pr.Merged), ClosedAt: forgeHistoryMilli(pr.Closed),
			Keys: trackerapp.TicketKeys(pr.Title + " " + pr.HeadRef), Acts: forgeHistoryActs(pr.Acts), ActsWhole: pr.ActsWhole,
		}
		read.PRs = append(read.PRs, row)
	}
	read.Acts = forgeHistoryActs(h.Acts)
	return read
}

func forgeHistoryActs(acts []gitops.PRHistoryAct) []store.ForgeHistoryAct {
	out := make([]store.ForgeHistoryAct, 0, len(acts))
	for _, act := range acts {
		kind := store.ForgeActCommented
		if act.Approved {
			kind = store.ForgeActApproved
		}
		out = append(out, store.ForgeHistoryAct{Number: act.Number, Kind: kind, Login: act.Person.Login,
			Name: cmp.Or(act.Person.Name, act.Person.Login), At: forgeHistoryMilli(act.At)})
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

package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/transport"
)

// The PR list pump: one per backend, the Pull requests page's list of the
// open PRs of every forge project (docs/specs/pull-requests.md, "The
// page"). It shares the PR pump's shape (pumpCore): refcounted handles
// tied to their connections, paused while every handle is hidden, one
// sequence. A tick reads each project with its forge's fixed request set
// and publishes, and stores, each project as its read lands
// (app_pr_list_tick.go). The store's copy (app_pr_list_store.go) answers a
// subscribe before any forge read, and the pump holds nothing once its
// last subscriber leaves.

// ErrTooManyPRListSubscriptions is returned once the list handle cap
// (maxPRUpdateHandles) is hit.
var ErrTooManyPRListSubscriptions = errors.New("pr list: too many active subscriptions")

// PRListSnapshot is the pulls:updated frame and SubscribePRList's
// snapshot: every forge project's rows, the needs-you count (the sidebar
// badge, each PR once), and Error when the projects themselves could not
// be read. Seq orders frames against a subscribe's snapshot; FetchedAt
// (RFC 3339) is when it was assembled. Stale is a snapshot served from the
// stored copy while a refresh runs: a subscribe's answer when a project's
// stored read is older than the PR update interval or missing, and the
// pump's frames until its first tick read every project.
type PRListSnapshot struct {
	Seq       uint64          `json:"seq"`
	FetchedAt string          `json:"fetchedAt"`
	NeedsYou  int             `json:"needsYou"`
	Stale     bool            `json:"stale"`
	Error     string          `json:"error,omitempty"`
	Projects  []ProjectPRList `json:"projects"`
}

// ProjectPRList is one project's rows, sorted (sortPRListItems). A failed
// read keeps the rows last read beside the failure: Error is the
// caller-safe sentence and ErrorKind, Reserve and ResumeAt its kind fields
// (PRUpdatedEvent's), ErrorDetails the correlation id and raw error behind
// it for "Details", and the other projects are unaffected. ReadAt (RFC
// 3339) is when the rows' read landed, "" before any did. DetailError is
// the caller-safe sentence of a detail request that failed while the
// list read: the rows list and the fields it fills read as unknown.
type ProjectPRList struct {
	ProjectID string               `json:"projectId"`
	Name      string               `json:"name"`
	Repo      gitops.RepoReference `json:"repo"`
	Rows      []PRListItem         `json:"rows"`
	ReadAt    string               `json:"readAt,omitempty"`
	Error     string               `json:"error,omitempty"`
	ErrorKind string               `json:"errorKind,omitempty"`
	// ErrorDetails is nil with no Error, and for a project whose origin
	// could not be read (Error is then that failure's own text).
	ErrorDetails *ForgeErrorDetail `json:"errorDetails,omitempty"`
	Reserve      bool              `json:"reserve,omitempty"`
	ResumeAt     string            `json:"resumeAt,omitempty"`
	DetailError  string            `json:"detailError,omitempty"`
}

// PRListItem is one row: the forge's row, its attention state, group and
// reason (prAttention), and the detail fields as the page shows them:
// Approvals given and required, Unresolved threads and CI, the head
// pipeline's normalized status; nil or "" while unknown.
type PRListItem struct {
	gitops.PRListRow
	State      string       `json:"state"`
	Group      string       `json:"group"`
	Reason     *PRReason    `json:"reason"`
	Approvals  *PRApprovals `json:"approvals"`
	Unresolved *int         `json:"unresolved"`
	CI         string       `json:"ci"`
}

// PRApprovals is a row's approvals standing and the approvals its base
// (GitHub) or project rules (GitLab) require.
type PRApprovals struct {
	Given    int `json:"given"`
	Required int `json:"required"`
}

// PRListBadge is the pulls:badge frame: the needs-you count of the stored
// snapshot, emitted whenever the store is written.
type PRListBadge struct {
	NeedsYou int `json:"needsYou"`
}

// PRListSubscriptionResult answers SubscribePRList.
type PRListSubscriptionResult struct {
	ID       string         `json:"id"`
	Snapshot PRListSnapshot `json:"snapshot"`
}

type prListPump struct {
	pumpCore
	// gen is the pump's listGen: it owns the stored copy until another
	// pump starts.
	gen uint64
	// The tick's state (app_pr_list_tick.go), under App.prUpdates.mu:
	// snapshot and encoded the last published frame, forge and forgeErr
	// the projects the running or last tick read, projects each
	// repository's rows and failure by key. seeded is the store's copy
	// loaded; landed every project of the first tick read once, which
	// ends Stale.
	snapshot PRListSnapshot
	encoded  []byte
	forge    []ForgeProject
	forgeErr error
	projects map[string]*prListProjectState
	seeded   bool
	landed   bool
}

type prListHandle struct {
	active bool
}

func prListCleanupKey(id string) string { return "pr-list:" + id }

// SubscribePRList starts the list pump, or joins it, and answers at once:
// with the pump's last frame when it published one, else the snapshot
// assembled from the stored copy (app_pr_list_store.go). The pump starts
// after that read, so its first tick, the user's action and so
// interactive, follows the answer with a frame per project as each read
// lands. The subscription ends with the calling
// connection. Routed to the selected computer like SubscribePRUpdates;
// the handle calls route home, and the caller pins them to the computer
// that answered.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SubscribePRList(ctx context.Context) (PRListSubscriptionResult, error) {
	if a.shuttingDown.Load() {
		return PRListSubscriptionResult{}, ErrShuttingDown
	}
	id, pump, start, published, seq, err := a.takePRListHandle()
	if err != nil {
		return PRListSubscriptionResult{}, err
	}
	if start {
		// The pump's first forge read follows the answer: it starts once
		// the stored copy is read, on every path out.
		defer a.prUpdates.wg.Go(func() { a.pumpPRList(pump) })
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		if !state.BindCleanup(prListCleanupKey(id), func() { a.unsubscribePRList(id) }) {
			a.unsubscribePRList(id)
			return PRListSubscriptionResult{}, errors.New("pr list: connection closing")
		}
	}
	if published != nil {
		return PRListSubscriptionResult{ID: id, Snapshot: *published}, nil
	}
	// The sequence was taken before the store is read: a frame the pump
	// publishes meanwhile is newer than what this read can see, and
	// outranks it.
	snapshot, err := a.storedPRListSnapshot(ctx)
	if err != nil {
		a.unsubscribePRList(id)
		return PRListSubscriptionResult{}, err
	}
	snapshot.Seq = seq
	return PRListSubscriptionResult{ID: id, Snapshot: snapshot}, nil
}

// takePRListHandle registers one handle on the live pump, creating it
// when there is none; start is true when the caller launches it.
// published is the pump's last frame when it has one; otherwise seq is
// the sequence the caller's stored snapshot carries.
func (a *App) takePRListHandle() (id string, pump *prListPump, start bool, published *PRListSnapshot, seq uint64, err error) {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	if a.prUpdates.listHandles == nil {
		a.prUpdates.listHandles = make(map[string]*prListHandle)
	}
	if len(a.prUpdates.listHandles) >= maxPRUpdateHandles {
		return "", nil, false, nil, 0, ErrTooManyPRListSubscriptions
	}
	pump = a.prUpdates.list
	if pump == nil || pump.dead {
		a.prUpdates.listGen++
		pump = &prListPump{pumpCore: a.newPumpCore(), gen: a.prUpdates.listGen, projects: map[string]*prListProjectState{}}
		a.prUpdates.list = pump
		start = true
	}
	id = uuid.NewString()
	pump.takeLocked()
	a.prUpdates.listHandles[id] = &prListHandle{active: true}
	if pump.snapshot.Seq != 0 {
		snapshot := pump.snapshot
		return id, pump, start, &snapshot, 0, nil
	}
	return id, pump, start, nil, a.nextPRUpdateSeqLocked(), nil
}

// SetPRListActive reports whether one subscriber shows the list; the pump
// reads nothing while every subscriber is hidden. An unknown id is a
// no-op.
//
//ao:scope git:operate
//ao:route home
func (a *App) SetPRListActive(ctx context.Context, subscriptionID string, active bool) error {
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.listHandles[subscriptionID]
	pump := a.prUpdates.list
	if !ok || handle.active == active || pump == nil || pump.dead {
		a.prUpdates.mu.Unlock()
		return nil
	}
	handle.active = active
	resumed := pump.voteLocked(active)
	a.prUpdates.mu.Unlock()
	if resumed {
		pump.wakeUp()
	}
	return nil
}

// RefreshPRList has the pump read every project now, as the user's
// action. Per-project failures are in the frames that follow.
//
//ao:scope git:operate
//ao:route home
func (a *App) RefreshPRList(ctx context.Context, subscriptionID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	a.prUpdates.mu.Lock()
	_, ok := a.prUpdates.listHandles[subscriptionID]
	pump := a.prUpdates.list
	a.prUpdates.mu.Unlock()
	if !ok || pump == nil || pump.dead {
		return errPRUpdatesUnknownSubscription
	}
	return a.requestPump(ctx, &pump.pumpCore, &prPumpRequest{snapshot: true})
}

//ao:scope git:operate
//ao:route home
func (a *App) UnsubscribePRList(ctx context.Context, subscriptionID string) error {
	transport.ConnStateFromContext(ctx).UnbindCleanup(prListCleanupKey(subscriptionID))
	a.unsubscribePRList(subscriptionID)
	return nil
}

func (a *App) unsubscribePRList(id string) {
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.listHandles[id]
	pump := a.prUpdates.list
	if !ok || pump == nil {
		a.prUpdates.mu.Unlock()
		return
	}
	delete(a.prUpdates.listHandles, id)
	var teardown *prListPump
	if pump.releaseLocked(handle.active) {
		teardown = pump
		a.prUpdates.list = nil
	}
	a.prUpdates.mu.Unlock()
	if teardown != nil {
		teardown.stop()
	}
}

// dropPRListPump forgets a pump whose goroutine exited, with every handle
// on it when it exited on its own, and drops what it held: the next
// subscribe reads the store.
func (a *App) dropPRListPump(pump *prListPump) {
	pump.cancel()
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	pump.dead = true
	if a.prUpdates.list == pump {
		a.prUpdates.list = nil
		clear(a.prUpdates.listHandles)
	}
	pump.snapshot, pump.encoded = PRListSnapshot{}, nil
	pump.forge, pump.forgeErr, pump.projects = nil, nil, nil
}

// closePRListPump releases every list handle; the caller joins the
// goroutine (closePRUpdatePumps).
func (a *App) closePRListPump() {
	a.prUpdates.mu.Lock()
	ids := make([]string, 0, len(a.prUpdates.listHandles))
	for id := range a.prUpdates.listHandles {
		ids = append(ids, id)
	}
	a.prUpdates.mu.Unlock()
	for _, id := range ids {
		a.unsubscribePRList(id)
	}
	a.prUpdates.mu.Lock()
	pump := a.prUpdates.list
	a.prUpdates.list = nil
	a.prUpdates.mu.Unlock()
	if pump != nil {
		pump.stop()
	}
}

func (a *App) pumpPRList(pump *prListPump) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("pr list: pump panic: %v", r)
		}
		a.dropPRListPump(pump)
	}()
	interval := a.prUpdatePollInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var retry retrySchedule
	missed := false
	tick := func(ctx context.Context) {
		missed = false
		failing, release := a.pollPRList(ctx, pump)
		// A failing project retries on the doubling schedule; one held by a
		// rate limit waits for its release, while the others ride the
		// ticker.
		retry.arm(failing, time.Time{}, a.prUpdateRetryBase(), interval)
		if !release.IsZero() && (retry.timer == nil || heldUntil(release) < retry.delay) {
			retry.timer = time.After(heldUntil(release))
		}
	}
	tick(forgeapi.WithInteractive(pump.ctx))
	for {
		select {
		case <-pump.done:
			return
		case <-pump.ctx.Done():
			return
		case req := <-pump.requests:
			tick(forgeapi.WithInteractive(pump.ctx))
			close(req.done)
		case <-pump.wake:
			if pump.paused.Load() || !missed {
				continue
			}
			tick(pump.ctx)
		case <-retry.timer:
			if pump.paused.Load() {
				retry.pausedRetry()
				missed = true
				continue
			}
			tick(pump.ctx)
		case <-ticker.C:
			if pump.paused.Load() {
				missed = true
				continue
			}
			tick(pump.ctx)
		}
	}
}

// emitPRListBadge emits the needs-you count of the stored copy.
func (a *App) emitPRListBadge(needsYou int) {
	a.emit(eventchan.PullsBadge, PRListBadge{NeedsYou: needsYou})
}

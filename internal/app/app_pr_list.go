package app

import (
	"context"
	"errors"
	"fmt"
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
// sequence. A tick reads each project's list, the detail of the rows the
// pages focus, each row's attention state and the user's pr_views, and
// emits pulls:updated when the snapshot changed (app_pr_list_tick.go).

// maxPRListFocus bounds the PR keys one handle may focus: a screen of rows
// and the needs-you set beyond it.
const maxPRListFocus = 60

// ErrTooManyPRListSubscriptions is returned once the list handle cap
// (maxPRUpdateHandles) is hit.
var ErrTooManyPRListSubscriptions = errors.New("pr list: too many active subscriptions")

// PRListSnapshot is the pulls:updated frame and SubscribePRList's
// snapshot: every forge project's rows, the needs-you count (the sidebar
// badge, each PR once), and Error when the projects themselves could not be read. Seq
// orders frames against a subscribe's snapshot; FetchedAt (RFC 3339) is
// when the tick that produced it read the forges.
type PRListSnapshot struct {
	Seq       uint64          `json:"seq"`
	FetchedAt string          `json:"fetchedAt"`
	NeedsYou  int             `json:"needsYou"`
	Error     string          `json:"error,omitempty"`
	Projects  []ProjectPRList `json:"projects"`
}

// ProjectPRList is one project's rows, sorted (sortPRListItems). A failed
// read keeps the rows last read beside the failure: Error is the
// caller-safe sentence and ErrorKind, Reserve and ResumeAt its kind fields
// (PRUpdatedEvent's), and the other projects are unaffected.
type ProjectPRList struct {
	ProjectID string               `json:"projectId"`
	Name      string               `json:"name"`
	Repo      gitops.RepoReference `json:"repo"`
	Rows      []PRListItem         `json:"rows"`
	Error     string               `json:"error,omitempty"`
	ErrorKind string               `json:"errorKind,omitempty"`
	Reserve   bool                 `json:"reserve,omitempty"`
	ResumeAt  string               `json:"resumeAt,omitempty"`
}

// PRListItem is one row: the forge's list row, its attention state and
// group, the focus read's detail (nil until it lands; DetailError is that
// read's last failure), and the user's last opening of the PR (Seen) with
// what changed since (Since); both nil when the app never opened it.
type PRListItem struct {
	gitops.PRListRow
	State       string        `json:"state"`
	Group       string        `json:"group"`
	Detail      *PRListDetail `json:"detail"`
	DetailError string        `json:"detailError,omitempty"`
	Seen        *PRSeen       `json:"seen"`
	Since       *PRSince      `json:"since"`
}

// PRSeen is the user's last opening of a PR (pr_views): when (unix
// milliseconds), at which head, and the comment and commit counts the
// list held then (nil when it held none).
type PRSeen struct {
	OpenedAt int64  `json:"openedAt"`
	HeadSHA  string `json:"headSHA"`
	Comments *int   `json:"comments"`
	Commits  *int   `json:"commits"`
}

// PRSince is what changed since PRSeen: the head moved, and how many
// comments and commits were added (0 when the count was not held).
type PRSince struct {
	HeadMoved   bool `json:"headMoved"`
	NewComments int  `json:"newComments"`
	NewCommits  int  `json:"newCommits"`
}

// PRListSubscriptionResult answers SubscribePRList.
type PRListSubscriptionResult struct {
	ID       string         `json:"id"`
	Snapshot PRListSnapshot `json:"snapshot"`
}

// PRListFocusResult answers SetPRListFocus with the keys it ignored
// because the list holds no such row.
type PRListFocusResult struct {
	Unknown []string `json:"unknown"`
}

type prListPump struct {
	pumpCore
	// ready closes once the first tick ran: a subscriber waits for it, so
	// every subscriber starts from a read snapshot.
	ready chan struct{}
	// focusWake asks the loop to read the detail of newly focused rows,
	// republish to rebuild the snapshot after the user opened a PR.
	focusWake chan struct{}
	republish chan struct{}
	// The tick's state (app_pr_list_tick.go), under App.prUpdates.mu:
	// forge and forgeErr are the projects the last tick read.
	snapshot PRListSnapshot
	encoded  []byte
	forge    []ForgeProject
	forgeErr error
	projects map[string]*prListProjectState
	details  map[string]*prListDetailState
}

type prListHandle struct {
	active bool
	focus  map[string]bool
}

func prListCleanupKey(id string) string { return "pr-list:" + id }

// SubscribePRList starts the list pump, or joins it, and returns its
// snapshot once the first tick read the forges (that tick is the user's
// action, so interactive). The subscription ends with the calling
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
	id, pump, start, err := a.takePRListHandle()
	if err != nil {
		return PRListSubscriptionResult{}, err
	}
	if start {
		a.prUpdates.wg.Go(func() { a.pumpPRList(pump) })
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		if !state.BindCleanup(prListCleanupKey(id), func() { a.unsubscribePRList(id) }) {
			a.unsubscribePRList(id)
			return PRListSubscriptionResult{}, errors.New("pr list: connection closing")
		}
	}
	select {
	case <-pump.ready:
	case <-pump.done:
		return PRListSubscriptionResult{}, errPRUpdatesUnknownSubscription
	case <-ctx.Done():
		a.unsubscribePRList(id)
		return PRListSubscriptionResult{}, ctx.Err()
	}
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	return PRListSubscriptionResult{ID: id, Snapshot: pump.snapshot}, nil
}

// takePRListHandle registers one handle on the live pump, creating it
// when there is none; start is true when the caller launches it.
func (a *App) takePRListHandle() (id string, pump *prListPump, start bool, err error) {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	if a.prUpdates.listHandles == nil {
		a.prUpdates.listHandles = make(map[string]*prListHandle)
	}
	if len(a.prUpdates.listHandles) >= maxPRUpdateHandles {
		return "", nil, false, ErrTooManyPRListSubscriptions
	}
	pump = a.prUpdates.list
	if pump == nil || pump.dead {
		pump = &prListPump{
			pumpCore:  a.newPumpCore(),
			ready:     make(chan struct{}),
			focusWake: make(chan struct{}, 1),
			republish: make(chan struct{}, 1),
			projects:  map[string]*prListProjectState{},
			details:   map[string]*prListDetailState{},
		}
		a.prUpdates.list = pump
		start = true
	}
	id = uuid.NewString()
	pump.takeLocked()
	a.prUpdates.listHandles[id] = &prListHandle{active: true}
	return id, pump, start, nil
}

// SetPRListActive reports whether one subscriber shows the list; the pump
// reads nothing while every subscriber is hidden. Only a visible
// subscriber's focus selects detail reads, so a change of a focusing
// subscriber recomputes the selection at once. An unknown id is a no-op.
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
	refocus := len(handle.focus) > 0
	a.prUpdates.mu.Unlock()
	if resumed {
		pump.wakeUp()
	}
	if refocus {
		select {
		case pump.focusWake <- struct{}{}:
		default:
		}
	}
	return nil
}

// SetPRListFocus replaces the PR keys (git.PRReference.Key) whose detail
// one subscriber wants read while it is visible: the rows on its screen
// and the needs-you set, at most maxPRListFocus. A key the list holds no
// row for is ignored and reported. A newly focused row's detail is read
// at once, not at the next tick.
//
//ao:scope git:operate
//ao:route home
func (a *App) SetPRListFocus(ctx context.Context, subscriptionID string, prKeys []string) (PRListFocusResult, error) {
	if len(prKeys) > maxPRListFocus {
		return PRListFocusResult{}, fmt.Errorf("pr list: %d rows focused, at most %d", len(prKeys), maxPRListFocus)
	}
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.listHandles[subscriptionID]
	pump := a.prUpdates.list
	if !ok || pump == nil || pump.dead {
		a.prUpdates.mu.Unlock()
		return PRListFocusResult{}, errPRUpdatesUnknownSubscription
	}
	known := pump.rowKeysLocked()
	focus := make(map[string]bool, len(prKeys))
	result := PRListFocusResult{Unknown: []string{}}
	fresh := false
	for _, key := range prKeys {
		if !known[key] {
			result.Unknown = append(result.Unknown, key)
			continue
		}
		focus[key] = true
		if pump.details[key] == nil {
			fresh = true
		}
	}
	handle.focus = focus
	a.prUpdates.mu.Unlock()
	if fresh {
		select {
		case pump.focusWake <- struct{}{}:
		default:
		}
	}
	return result, nil
}

// RefreshPRList has the pump read every project and the focused rows'
// detail now, as the user's action. Per-project failures are in the
// snapshot that follows.
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
	// A visible handle's focus leaves with it: the others' rows may take
	// its share of the detail allowance now.
	refocus := teardown == nil && handle.active && len(handle.focus) > 0
	a.prUpdates.mu.Unlock()
	if teardown != nil {
		teardown.stop()
	}
	if refocus {
		select {
		case pump.focusWake <- struct{}{}:
		default:
		}
	}
}

// dropPRListPump forgets a pump whose goroutine exited on its own, with
// every handle on it.
func (a *App) dropPRListPump(pump *prListPump) {
	pump.cancel()
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	pump.dead = true
	if a.prUpdates.list == pump {
		a.prUpdates.list = nil
		clear(a.prUpdates.listHandles)
	}
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
	tick := func(ctx context.Context, forceDetails bool) {
		missed = false
		event, changed, failing, release := a.pollPRList(ctx, pump, forceDetails)
		// A failing project retries on the doubling schedule; one held by a
		// rate limit waits for its release, while the others ride the
		// ticker.
		retry.arm(failing, time.Time{}, a.prUpdateRetryBase(), interval)
		if !release.IsZero() && (retry.timer == nil || heldUntil(release) < retry.delay) {
			retry.timer = time.After(heldUntil(release))
		}
		if changed {
			a.emitPumpEvent(&pump.pumpCore, eventchan.PullsUpdated, event)
		}
	}
	tick(forgeapi.WithInteractive(pump.ctx), false)
	close(pump.ready)
	for {
		select {
		case <-pump.done:
			return
		case <-pump.ctx.Done():
			return
		case req := <-pump.requests:
			tick(forgeapi.WithInteractive(pump.ctx), true)
			close(req.done)
		case <-pump.focusWake:
			if pump.paused.Load() {
				missed = true
				continue
			}
			if event, changed := a.pollPRListFocus(forgeapi.WithInteractive(pump.ctx), pump); changed {
				a.emitPumpEvent(&pump.pumpCore, eventchan.PullsUpdated, event)
			}
		case <-pump.republish:
			if event, changed, _, _ := a.publishPRList(pump); changed {
				a.emitPumpEvent(&pump.pumpCore, eventchan.PullsUpdated, event)
			}
		case <-pump.wake:
			if pump.paused.Load() || !missed {
				continue
			}
			tick(pump.ctx, false)
		case <-retry.timer:
			if pump.paused.Load() {
				retry.pausedRetry()
				missed = true
				continue
			}
			tick(pump.ctx, false)
		case <-ticker.C:
			if pump.paused.Load() {
				missed = true
				continue
			}
			tick(pump.ctx, false)
		}
	}
}

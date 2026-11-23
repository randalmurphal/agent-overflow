package app

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// Each device with a listening connection owns a bounded, volatile log.
// Its first observation reconciles changes made without a listener.
const ownerChangeLimit = 256

var ownerWaitBound = transport.MethodBudget("ThreadOwnerChanges") - transport.MethodAnswerMargin

// OwnerCursor records the activity and structural changes already received.
type OwnerCursor struct {
	// AfterRevision is the largest structural revision recorded as owed.
	AfterRevision int64 `json:"afterRevision,omitempty"`
	// Launch identifies the process that published Version.
	Launch string `json:"launch"`
	// Version is this device's activity watermark.
	Version uint64 `json:"version"`
}

// OwnerChanges describes new obligations since the calling device's cursor.
type OwnerChanges struct {
	// OwnerCursor covers every change included in this answer.
	OwnerCursor

	// Tokens lists receipts that need a fresh state read.
	Tokens []string `json:"tokens,omitempty"`

	// PromptTokens is the subset whose prompt routing changed.
	PromptTokens []string `json:"promptTokens,omitempty"`

	// Structural carries unacknowledged edits in issuance order.
	Structural []store.SpawnRevision `json:"structural,omitempty"`

	// Reset requires recovery before this computer can attest to rest.
	Reset bool `json:"reset,omitempty"`
}

type ownerActivity struct {
	mu      sync.Mutex
	devices map[string]*deviceActivity
}

type deviceActivity struct {
	// tie binds the log's end to the latest connection that asked for it.
	tie                transport.Tie
	version, discarded uint64
	entries            map[string]ownerEntry
	changed            chan struct{}
}

type ownerEntry struct {
	version uint64
	prompt  bool
}

func (o *ownerActivity) watching() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.devices) > 0
}

const ownerActivitySubscription = "thread-owner-activity"

// dropDevice discards activity as soon as revocation commits, even while the
// transport is still closing its sockets.
func (o *ownerActivity) dropDevice(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if d := o.devices[id]; d != nil {
		delete(o.devices, id)
		d.tie.Untie()
		d.wake()
	}
}

// watchLocked ties the log to the latest connection that asked for it, under
// the revocation lock its release takes, in the same critical section that
// publishes the log. When that connection ends, the next listener starts
// with a full reconciliation. A connection that is closing is tied to
// nothing and publishes nothing.
func (o *ownerActivity) watchLocked(id string, socket *transport.ConnState) (*deviceActivity, bool, error) {
	d := o.devices[id]
	first := d == nil
	if first {
		d = &deviceActivity{entries: make(map[string]ownerEntry), changed: make(chan struct{})}
	}
	if !d.tie.HeldBy(socket) {
		tie, err := socket.Bind(ownerActivitySubscription, func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			if o.devices[id] == d && d.tie.HeldBy(socket) {
				delete(o.devices, id)
				d.wake()
			}
		})
		if err != nil {
			return nil, false, err
		}
		d.tie.Untie()
		d.tie = tie
	}
	if first {
		if o.devices == nil {
			o.devices = make(map[string]*deviceActivity)
		}
		o.devices[id] = d
	}
	return d, first, nil
}

func (d *deviceActivity) wake() {
	close(d.changed)
	d.changed = make(chan struct{})
}

func (o *ownerActivity) note(device, token string, prompt bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.devices[device]
	if d == nil {
		return
	}
	d.version++
	d.entries[token] = ownerEntry{version: d.version, prompt: prompt || d.entries[token].prompt}
	if len(d.entries) > ownerChangeLimit {
		oldest := token
		for candidate, entry := range d.entries {
			if entry.version < d.entries[oldest].version {
				oldest = candidate
			}
		}
		d.discarded = d.entries[oldest].version
		delete(d.entries, oldest)
	}
	d.wake()
}

// An unresolved change can concern any owner. Existing cursors must reconcile,
// including a device whose first wait starts after resolution failed.
func (o *ownerActivity) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, d := range o.devices {
		d.version++
		d.discarded = d.version
		clear(d.entries)
		d.wake()
	}
}

func (o *ownerActivity) snapshot(d *deviceActivity, launch string, cursor OwnerCursor) (OwnerChanges, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	answer := OwnerChanges{OwnerCursor: OwnerCursor{Launch: launch, Version: d.version}}
	answer.Reset = cursor.Launch != launch || cursor.Version < d.discarded || cursor.Version > d.version
	if !answer.Reset {
		for token, entry := range d.entries {
			if entry.version <= cursor.Version {
				delete(d.entries, token)
				d.discarded = max(d.discarded, entry.version)
				continue
			}
			answer.Tokens = append(answer.Tokens, token)
			if entry.prompt {
				answer.PromptTokens = append(answer.PromptTokens, token)
			}
		}
		slices.Sort(answer.Tokens)
		slices.Sort(answer.PromptTokens)
	}
	return answer, d.changed
}

// Authentication and installation share the revocation lock so a call admitted
// before revocation cannot reinstall the device after its log is discarded.
func (a *App) watchOwnerConnection(ctx context.Context, conn *transport.ConnState) (string, *deviceActivity, bool, error) {
	o := &a.ownerActivity
	o.mu.Lock()
	defer o.mu.Unlock()
	device, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return "", nil, false, err
	}
	if device == threadReceiptLocalOwner {
		return "", nil, false, fmt.Errorf("owner changes require a paired connection")
	}
	d, first, err := o.watchLocked(device, conn)
	return device, d, first, err
}

// ThreadOwnerChanges waits for changes to the calling device's work.
//
//ao:scope threads:read
//ao:route selected
//ao:budget 1m
//ao:callerbound
func (a *App) ThreadOwnerChanges(ctx context.Context, cursor OwnerCursor) (OwnerChanges, error) {
	conn, err := transport.CallerConnection(ctx)
	if err != nil {
		return OwnerChanges{}, err
	}
	server := a.transportServer.Load()
	if server == nil {
		return OwnerChanges{}, fmt.Errorf("owner changes require a paired connection")
	}
	ctx, cancel := context.WithTimeout(ctx, ownerWaitBound)
	defer cancel()
	device, d, first, err := a.watchOwnerConnection(ctx, conn)
	if err != nil {
		return OwnerChanges{}, err
	}
	if first {
		cursor = OwnerCursor{}
	}
	for {
		if err := ctx.Err(); err != nil {
			return OwnerChanges{}, err
		}
		answer, changed := a.ownerActivity.snapshot(d, server.LaunchID(), cursor)
		answer.AfterRevision = cursor.AfterRevision
		if answer.Reset {
			answer.AfterRevision = 0
		}
		answer.Structural, err = a.store.SpawnRevisionsAfter(device, answer.AfterRevision, threadPeerTokenLimit)
		if err != nil {
			return OwnerChanges{}, err
		}
		if len(answer.Structural) > 0 {
			answer.AfterRevision = answer.Structural[len(answer.Structural)-1].Revision
		}
		if answer.Reset || len(answer.Tokens)+len(answer.Structural) > 0 {
			return answer, nil
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return answer, nil
			}
			return OwnerChanges{}, ctx.Err()
		case <-changed:
		}
	}
}

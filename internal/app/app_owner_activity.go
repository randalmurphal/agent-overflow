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
	// PromptsOnly reads priority changes without acknowledging ordinary activity.
	PromptsOnly bool `json:"promptsOnly,omitempty"`
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
	socket             *transport.ConnState
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

// watch ties the log to the latest connection that asked for it. When that
// connection ends, the next listener starts with a full reconciliation.
func (o *ownerActivity) watch(id string, socket *transport.ConnState) (*deviceActivity, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.devices == nil {
		o.devices = make(map[string]*deviceActivity)
	}
	d := o.devices[id]
	first := d == nil
	if first {
		d = &deviceActivity{entries: make(map[string]ownerEntry), changed: make(chan struct{})}
		o.devices[id] = d
	}
	if d.socket != socket {
		const subscription = "thread-owner-activity"
		if !socket.BindCleanup(subscription, func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			if o.devices[id] == d && d.socket == socket {
				delete(o.devices, id)
				d.wake()
			}
		}) {
			if first {
				delete(o.devices, id)
			}
			return nil, false, context.Canceled
		}
		if d.socket != nil {
			d.socket.UnbindCleanup(subscription)
		}
		d.socket = socket
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
				if !cursor.PromptsOnly {
					delete(d.entries, token)
					d.discarded = max(d.discarded, entry.version)
				}
				continue
			}
			if cursor.PromptsOnly && !entry.prompt {
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

// ThreadOwnerChanges waits for changes to the calling device's work.
//
//ao:scope threads:read
//ao:route selected
//ao:budget 1m
//ao:callerbound
func (a *App) ThreadOwnerChanges(ctx context.Context, cursor OwnerCursor) (OwnerChanges, error) {
	device, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return OwnerChanges{}, err
	}
	conn := transport.ConnStateFromContext(ctx)
	server := a.transportServer.Load()
	if device == threadReceiptLocalOwner || conn == nil || server == nil {
		return OwnerChanges{}, fmt.Errorf("owner changes require a paired connection")
	}
	ctx, cancel := context.WithTimeout(ctx, ownerWaitBound)
	defer cancel()
	d, first, err := a.ownerActivity.watch(device, conn)
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
		if !cursor.PromptsOnly {
			answer.Structural, err = a.store.SpawnRevisionsAfter(device, answer.AfterRevision, threadPeerTokenLimit)
			if err != nil {
				return OwnerChanges{}, err
			}
			if len(answer.Structural) > 0 {
				answer.AfterRevision = answer.Structural[len(answer.Structural)-1].Revision
			}
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

package app

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

const ownerChangeLimit = 256
const ownerWaitBound = 40 * time.Second

type OwnerCursor struct {
	Launch  string `json:"launch"`
	Version uint64 `json:"version"`
}

type OwnerChanges struct {
	OwnerCursor
	Tokens       []string              `json:"tokens,omitempty"`
	PromptTokens []string              `json:"promptTokens,omitempty"`
	Structural   []store.SpawnRevision `json:"structural,omitempty"`
	Reset        bool                  `json:"reset,omitempty"`
}

type ownerActivity struct {
	mu      sync.Mutex
	devices map[string]*deviceActivity
}

type deviceActivity struct {
	version, discarded uint64
	entries            map[string]ownerEntry
	changed            chan struct{}
}

type ownerEntry struct {
	version uint64
	prompt  bool
}

func (o *ownerActivity) deviceLocked(id string) *deviceActivity {
	if o.devices == nil {
		o.devices = make(map[string]*deviceActivity)
	}
	d := o.devices[id]
	if d == nil {
		d = &deviceActivity{entries: make(map[string]ownerEntry), changed: make(chan struct{})}
		o.devices[id] = d
	}
	return d
}

func (d *deviceActivity) wake() {
	close(d.changed)
	d.changed = make(chan struct{})
}

func (o *ownerActivity) note(device, token string, prompt bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.deviceLocked(device)
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

func (o *ownerActivity) snapshot(device, launch string, cursor OwnerCursor) (OwnerChanges, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.deviceLocked(device)
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
	key := uuid.NewString()
	if !conn.BindCleanup(key, cancel) {
		return OwnerChanges{}, context.Canceled
	}
	defer conn.UnbindCleanup(key)
	for {
		answer, changed := a.ownerActivity.snapshot(device, server.LaunchID(), cursor)
		answer.Structural, err = a.store.PendingSpawnRevisions(device, "", threadPeerTokenLimit)
		if err != nil {
			return OwnerChanges{}, err
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

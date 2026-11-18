package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// Owner updates supervise one reconciled peer session per paired computer with work.
// A failed session retains every token it has yet to apply.
type ownerUpdates struct {
	workers workGroup
	mu      sync.Mutex
	peers   map[string]*ownerPeer
}

type ownerPeer struct {
	origin       attachedbackends.PeerRead
	cancel       context.CancelFunc
	done         chan struct{}
	reprobe      chan struct{}
	legacy       bool
	ready        bool
	owed         map[string]ownerObligation
	launch       string
	disconnected <-chan struct{}
}

// refreshOwnerPeers starts one wait for each pairing that has work here.
func (a *App) refreshOwnerPeers() error {
	if !buildvariant.RemoteAccess || a.backends == nil {
		return nil
	}
	ids, err := a.store.OwnerComputers()
	if err != nil {
		return err
	}
	o := &a.ownerUpdates
	o.mu.Lock()
	for id, p := range o.peers {
		if _, has := slices.BinarySearch(ids, id); !has {
			p.cancel()
		}
	}
	o.mu.Unlock()
	var failures []error
	for _, id := range ids {
		unlock := a.lockComputerPairing(id)
		paired, err := a.pairingSaved(id)
		if err != nil {
			unlock()
			failures = append(failures, fmt.Errorf("computer %s: %w", id, err))
			continue
		}
		if !paired {
			unlock()
			// A missing profile may be a removal whose notification was lost.
			// Historical links alone need no repeated pairing-end transaction.
			o.mu.Lock()
			running := o.peers[id] != nil
			o.mu.Unlock()
			keys, err := a.store.RecoverOwnerRequests(id, "", 1)
			if err == nil && (running || len(keys) != 0) {
				_, err = a.endUnpaired(id)
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("end owner work on %s: %w", id, err))
			}
			continue
		}
		o := &a.ownerUpdates
		o.mu.Lock()
		if o.peers == nil {
			o.peers = map[string]*ownerPeer{}
		}
		p := o.peers[id]
		if p == nil {
			ctx, cancel := context.WithCancel(a.lifeCtx())
			p = &ownerPeer{cancel: cancel, done: make(chan struct{}), reprobe: make(chan struct{}, 1)}
			o.peers[id] = p
			if !o.workers.Go(func() {
				defer close(p.done)
				defer func() {
					o.mu.Lock()
					if o.peers[id] == p {
						delete(o.peers, id)
					}
					o.mu.Unlock()
					a.nudgeThreadRequestSweep()
				}()
				defer cancel()
				maintained := make(chan struct{})
				go func() { defer close(maintained); a.maintainOwnerPeer(ctx, id, p) }()
				a.runOwnerPeer(ctx, id, p)
				cancel()
				<-maintained
			}) {
				delete(o.peers, id)
				cancel()
				close(p.done)
			}
		}
		o.mu.Unlock()
		unlock()
	}
	return errors.Join(failures...)
}

func (a *App) runOwnerPeer(ctx context.Context, id string, p *ownerPeer) {
	delay := a.threadPollNormal()
	for ctx.Err() == nil {
		err := a.runOwnerSession(ctx, id, p)
		if ctx.Err() != nil {
			return
		}
		a.ownerUpdates.mu.Lock()
		p.ready = false
		p.legacy = attachedbackends.Unsupported(err)
		legacy := p.legacy
		hadLaunch := p.launch != ""
		a.ownerUpdates.mu.Unlock()
		if errors.Is(err, errOwnerCursorReset) {
			a.invalidateOwnerTrees(id, nil)
			continue
		}
		if !legacy || hadLaunch {
			a.invalidateOwnerTrees(id, err)
		}
		if legacy {
			delay = threadRequestExpiryInterval
		} else {
			log.Printf("owner updates on %s: retrying after %s: %v", id, delay, err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.reprobe:
			timer.Stop()
		case <-timer.C:
		}
		delay = min(2*delay, threadAgentPeerBackoffMax)
	}
}

func (a *App) invalidateOwnerTrees(computer string, failure error) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	var callers []string
	for _, entry := range c.entries {
		if entry.spawn.TargetComputerID != computer {
			continue
		}
		entry.at = time.Time{}
		entry.listing.Placement = ""
		if failure != nil {
			entry.err = failure
		}
		callers = append(callers, entry.spawn.CallerThreadID)
	}
	c.mu.Unlock()
	for _, caller := range callers {
		a.projectPeerRows(caller)
	}
}

func (a *App) recoverOwnerPeer(ctx context.Context, id, launch string) error {
	if held := a.workerPrompts.remote.heldLinks(id); len(held) > 0 {
		if err := a.readOwnerChanges(ctx, id, launch, held, nil); err != nil {
			return err
		}
	}
	for key := (&OwnerPageKey{}); key != nil; {
		var page OwnerRecoveryPage
		call, cancel := context.WithTimeout(ctx, threadPollCallTimeout)
		origin, err := a.backends.ReadThreadPeer(call, id, transport.CapabilityThreadToolsV2, "", "ThreadOwnerRecovery", &page, launch, *key)
		cancel()
		if err != nil {
			return err
		}
		if err := origin.Apply(launch, func() error { return ctx.Err() }); err != nil {
			return err
		}
		tokens := append(page.Tokens, ownerRevisionTokens(page.Structural)...)
		a.ownerReads.changed(tokens)
		if err = a.readOwnerChanges(ctx, id, launch, tokens, page.Structural); err != nil {
			return err
		}
		a.invalidateChangedOwnerTrees(id, tokens)
		key = page.Next
	}
	for after := ""; ; {
		keys, err := a.store.RecoverOwnerRequests(id, after, ownerPageSize)
		if err != nil {
			return err
		}
		if err = a.readOwnerChanges(ctx, id, launch, keys, nil); err != nil {
			return err
		}
		a.invalidateChangedOwnerTrees(id, keys)
		if len(keys) < ownerPageSize {
			return nil
		}
		after = keys[len(keys)-1]
	}
}

func ownerRevisionTokens(changes []store.SpawnRevision) []string {
	tokens := make([]string, len(changes))
	for i, change := range changes {
		tokens[i] = change.Token
	}
	return tokens
}

func (a *App) readOwnerChanges(ctx context.Context, id, launch string, keys []string, structural []store.SpawnRevision) error {
	seen := make(map[string]bool, len(keys))
	var tokens []string
	for _, token := range keys {
		if !seen[token] {
			seen[token] = true
			tokens = append(tokens, token)
		}
	}
	for _, edit := range structural {
		if !seen[edit.Token] {
			seen[edit.Token] = true
			tokens = append(tokens, edit.Token)
		}
	}
	for start := 0; start < len(tokens); start += ownerPageSize {
		chunk := tokens[start:min(start+ownerPageSize, len(tokens))]
		for {
			err := a.readOwnerBatch(ctx, id, launch, chunk, structural)
			if errors.Is(err, errReadSuperseded) && ctx.Err() == nil {
				continue
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

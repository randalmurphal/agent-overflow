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

type ownerUpdates struct {
	workers workGroup
	mu      sync.Mutex
	peers   map[string]*ownerPeer
}

type ownerPeer struct {
	cancel       context.CancelFunc
	done         chan struct{}
	reprobe      chan struct{}
	legacy       bool
	ready        bool
	owed         []string
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
	var cursor OwnerCursor
	delay := a.threadPollNormal()
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, ownerWaitBound+10*time.Second)
		var answer OwnerChanges
		origin, err := a.backends.ReadThreadPeer(call, id, transport.CapabilityThreadToolsV2, "", "ThreadOwnerChanges", &answer, cursor)
		cancel()
		if err == nil {
			err = origin.Apply(answer.Launch, func() error {
				o := &a.ownerUpdates
				o.mu.Lock()
				defer o.mu.Unlock()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.launch, p.legacy = answer.Launch, false
				p.disconnected = origin.Disconnected()
				if answer.Reset {
					p.ready = false
				} else {
					p.owed = append(slices.Clone(answer.Tokens), ownerRevisionTokens(answer.Structural)...)
					slices.Sort(p.owed)
					p.owed = slices.Compact(p.owed)
				}
				return nil
			})
		}
		if err == nil {
			tokens := append(slices.Clone(answer.PromptTokens), p.owed...)
			a.ownerReads.changed(tokens)
			if answer.Reset {
				a.invalidateOwnerTrees(id, nil)
				err = a.recoverOwnerPeer(ctx, id, answer.Launch)
				// Recovery consumed the current structural revisions. The wait may have observed an older edit.
				answer.Structural = nil
			}
			if err == nil {
				err = a.readOwnerChanges(ctx, id, answer.Launch, tokens, answer.Structural)
				if err == nil {
					a.invalidateChangedOwnerTrees(id, tokens)
				}
			}
			if err == nil {
				err = origin.Apply(answer.Launch, func() error {
					a.ownerUpdates.mu.Lock()
					p.ready = true
					p.owed = nil
					a.ownerUpdates.mu.Unlock()
					cursor = answer.OwnerCursor
					return nil
				})
			}
		}
		if err == nil {
			delay = a.threadPollNormal()
			continue
		}
		if ctx.Err() != nil {
			return
		}
		a.ownerUpdates.mu.Lock()
		p.ready = false
		p.legacy = attachedbackends.Unsupported(err)
		legacy := p.legacy
		a.ownerUpdates.mu.Unlock()
		if !legacy {
			a.invalidateOwnerTrees(id, err)
		}
		cursor = OwnerCursor{}
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

func (a *App) readOwnerBatch(ctx context.Context, id, launch string, keys []string, structural []store.SpawnRevision) error {
	read := a.ownerReads.start(keys)
	defer read.close()
	rows, err := a.store.OwnerRequests(id, keys)
	if err != nil {
		return err
	}
	poll := ThreadPeerPoll{Launch: launch}
	spawns := map[string]store.ThreadRequest{}
	for _, row := range rows {
		if row.RemoteListingReach() || (row.Kind == store.ThreadRequestSpawn && row.LinkEnd == "" && row.TargetThreadID == "" && !threadRequestSettled(row)) {
			spawns[row.Token] = row
			poll.Links = append(poll.Links, OwnerLinkRead{Token: row.Token, Source: row.MadeIn(), Held: a.heldRemotePrompts(row)})
		}
		if row.Polling && !row.LineageOnly {
			poll.Tokens = append(poll.Tokens, row.Token)
			if row.Revision > 0 {
				poll.Held = append(poll.Held, ThreadPeerAck{Token: row.Token, Revision: row.Revision})
			}
		}
	}
	call, cancel := context.WithTimeout(ctx, threadPollCallTimeout)
	defer cancel()
	if len(rows) == 0 {
		for _, edit := range structural {
			if slices.Contains(keys, edit.Token) {
				poll.StructuralAck = append(poll.StructuralAck, edit.SpawnRevisionAck)
			}
		}
		if len(poll.StructuralAck) == 0 {
			return nil
		}
		return a.backends.CallThreadPeerOn(call, id, transport.CapabilityThreadToolsV2, "ThreadToolRequestStatus", &ThreadPeerPollReply{}, poll)
	}
	var reply ThreadPeerPollReply
	started := time.Now()
	origin, err := a.backends.ReadThreadPeer(call, id, transport.CapabilityThreadToolsV2, "", "ThreadToolRequestStatus", &reply, poll)
	if err != nil {
		return err
	}
	ends := map[string]string{}
	var acks []store.SpawnRevisionAck
	var wakes []threadWakeRef
	var promptResults []<-chan error
	unlock := a.lockComputerPairing(id)
	err = read.apply(func() error {
		return origin.Apply(launch, func() error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			for _, edit := range structural {
				if !slices.Contains(keys, edit.Token) {
					continue
				}
				if _, err := a.store.ApplySpawnRevision(id, edit); err != nil {
					return err
				}
				acks = append(acks, edit.SpawnRevisionAck)
				ends[edit.Token] = edit.End
			}
			if len(reply.Requests) != len(poll.Tokens) || len(reply.Links) != len(poll.Links) {
				return errors.New("incomplete owner read")
			}
			for i, answer := range reply.Requests {
				if answer.Token != poll.Tokens[i] {
					return fmt.Errorf("unexpected status token %q", answer.Token)
				}
				row, found, err := a.store.GetThreadRequest(answer.Token)
				if err != nil {
					return err
				}
				if !found || row.TargetComputerID != id {
					continue
				}
				if !answer.Known && row.State == store.ThreadRequestUnconfirmed {
					// Only the dispatch clock can rule out a request still on its way.
					continue
				}
				w, err := a.applyKnownPeerRow(row, answer, threadPeerHearing{at: started, v2: true})
				wakes = append(wakes, w...)
				if err != nil {
					return err
				}
				if answer.Known && store.ThreadReceiptSettled(answer.State) {
					current, found, err := a.store.GetThreadRequest(answer.Token)
					if err != nil {
						return err
					}
					if found && !threadLateReplyAwaited(current, started) {
						if _, err := a.store.RetireThreadRequestPoll(answer.Token); err != nil {
							return err
						}
					}
				}
			}
			for i, state := range reply.Links {
				if state.Token != poll.Links[i].Token {
					return fmt.Errorf("unexpected link token %q", state.Token)
				}
				if state.End == "pending" {
					continue
				}
				spawn := spawns[state.Token]
				if spawn.TargetThreadID == "" {
					current, found, err := a.store.GetThreadRequest(state.Token)
					if err != nil {
						return err
					}
					if !found || current.TargetThreadID == "" {
						continue
					}
					spawn = current
				}
				if state.State == nil && (ends[state.Token] == "gone" || ends[state.Token] == "detached") {
					state.End = ends[state.Token]
				}
				revision := store.SpawnRevision{SpawnRevisionAck: store.SpawnRevisionAck{Token: state.Token}, End: state.End}
				if state.State != nil {
					revision.Title = state.State.Title
				}
				if _, err := a.store.ApplySpawnRevision(id, revision); err != nil {
					return err
				}
				listing := ThreadAgentRows{}
				if state.State != nil {
					listing = *state.State
				}
				if _, err := a.store.SetThreadRequestNestedOpen(state.Token, peerListingOpen(listing)); err != nil {
					return err
				}
				if done := a.noteRemotePrompts(origin, spawn, listing, nil); done != nil {
					promptResults = append(promptResults, done)
				}
				c := &a.threadAgentPeers
				c.mu.Lock()
				changed := c.noteAnswerLocked(spawn, listing, nil, true)
				c.mu.Unlock()
				if changed {
					a.projectPeerRows(spawn.CallerThreadID)
				}
			}
			return nil
		})
	})
	unlock()
	a.deliverThreadWakes(wakes)
	if err != nil {
		return err
	}
	for _, done := range promptResults {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if len(reply.Requests) > 0 {
		a.sendOwedThreadRequestAcks(id)
	}
	if len(acks) > 0 {
		return a.backends.CallThreadPeerOn(call, id, transport.CapabilityThreadToolsV2, "ThreadToolRequestStatus", &ThreadPeerPollReply{}, ThreadPeerPoll{Launch: launch, StructuralAck: acks})
	}
	return nil
}

package app

import (
	"context"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

func (o *ownerUpdates) contact(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if p := o.peers[id]; p != nil && p.legacy {
		select {
		case p.reprobe <- struct{}{}:
		default:
		}
	}
}

func (o *ownerUpdates) cancelPeer(id string) <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	if p := o.peers[id]; p != nil {
		p.ready = false
		p.cancel()
		return p.done
	}
	return nil
}

func (a *App) invalidateChangedOwnerTrees(id string, tokens []string) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	for _, token := range tokens {
		if e := c.entries[token]; e != nil && e.spawn.TargetComputerID == id {
			e.at = time.Time{}
		}
	}
	c.mu.Unlock()
	for _, token := range tokens {
		a.noteThreadRequestChanged(store.ThreadRequestChange{Token: token})
	}
}

func (p *remoteWorkerPrompt) isHeld() bool {
	return p.state == remotePromptAnswering || (p.state == remotePromptRouted && p.held)
}

func (r *remoteWorkerPrompts) heldLinks(computer string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for key, p := range r.byKey {
		if key.computerID == computer && (p.state == remotePromptRouted || p.state == remotePromptAnswering || ((p.state == remotePromptClosed || p.state == remotePromptReturned) && p.delivery != "")) {
			out = append(out, p.spawn.Token)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Held prompts keep their lease while a wait reconnects or recovery pages
// are being read. Each pass can also prove one batch of overdue links.
func (a *App) maintainOwnerPeer(ctx context.Context, id string, p *ownerPeer) {
	timer := time.NewTicker(a.threadPollNormal())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		a.ownerUpdates.mu.Lock()
		launch, legacy := p.launch, p.legacy
		a.ownerUpdates.mu.Unlock()
		if launch == "" || legacy {
			continue
		}
		keys := a.workerPrompts.remote.heldLinks(id)
		if err := a.readOwnerChanges(ctx, id, launch, keys, nil); err != nil && ctx.Err() == nil {
			log.Printf("owner prompt renewal on %s will retry: %v", id, err)
		}
		if err := a.proveDueLinks(ctx, id, launch, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("owner link contact on %s will retry: %v", id, err)
		}
	}
}

func (a *App) proveDueLinks(ctx context.Context, id, launch string, now time.Time) error {
	rows, err := a.store.DueLinkContacts(id, now.UnixMilli(), ownerPageSize)
	if err != nil || len(rows) == 0 {
		return err
	}
	var keys []string
	poll := ThreadPeerPoll{Launch: launch}
	for _, row := range rows {
		keys = append(keys, row.Token)
		poll.Contacts = append(poll.Contacts, OwnerLinkRead{Token: row.Token, Source: row.MadeIn()})
	}
	call, cancel := context.WithTimeout(ctx, threadPollCallTimeout)
	defer cancel()
	origin, err := a.backends.ReadThreadPeer(call, id, transport.CapabilityThreadToolsV2, "", "ThreadToolRequestStatus", &ThreadPeerPollReply{}, poll)
	if err != nil {
		return err
	}
	return origin.Apply(launch, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return a.store.ScheduleLinkContacts(id, keys, now.Add(24*time.Hour).UnixMilli())
	})
}

func (o *ownerUpdates) capable() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var ids []string
	for id, p := range o.peers {
		if p.launch != "" && !p.legacy {
			ids = append(ids, id)
		}
	}
	return ids
}

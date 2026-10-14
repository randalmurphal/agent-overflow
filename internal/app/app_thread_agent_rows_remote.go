package app

import (
	"context"
	"errors"
	"log"
	"reflect"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// The Agents rows a spawn on a paired computer nests, as this computer last
// heard them. A pane's read never waits on another computer: it nests the
// last agent_rows answer for each remote spawn and asks again in the
// background, and an answer that changes anything emits
// `thread:agent_rows_changed` for the spawn's caller, so the pane reads
// again. Every answer also records on the spawn's row whether that
// computer listed open work (`nested_open`), which keeps a settled remote
// spawn listed while it does; the poller asks again at its own pace while
// a row says so.

const (
	// threadAgentPeerListsInFlight bounds the agent_rows calls in flight
	// at once, across every listing.
	threadAgentPeerListsInFlight = 8
	// threadAgentPeerRefreshEvery is the shortest gap between two reads of
	// one spawn's rows a pane's reads start: the frame an answer emits
	// brings the pane back, and that read must not ask again.
	threadAgentPeerRefreshEvery = time.Second
	// threadAgentPeerCacheLimit bounds the remembered answers. The oldest
	// answer goes first; a spawn still listed is asked again.
	threadAgentPeerCacheLimit = 256
)

// threadAgentPeerCache is the last agent_rows answer for each remote spawn,
// by the spawn's token.
type threadAgentPeerCache struct {
	mu      sync.Mutex
	entries map[string]*threadAgentPeerEntry
	slots   chan struct{}
	// sweepAfter is the last spawn the sweep asked about; the next sweep
	// starts after it, and from the start once a page comes back short.
	sweepAfter store.ThreadRequest
}

type threadAgentPeerEntry struct {
	spawn    store.ThreadRequest
	listing  ThreadAgentRows
	err      error
	answered bool
	at       time.Time
	inFlight bool
}

// entryLocked returns the token's entry, made if missing, making room.
func (c *threadAgentPeerCache) entryLocked(spawn store.ThreadRequest) *threadAgentPeerEntry {
	if c.entries == nil {
		c.entries = map[string]*threadAgentPeerEntry{}
	}
	if entry, ok := c.entries[spawn.Token]; ok {
		entry.spawn = spawn
		return entry
	}
	if len(c.entries) >= threadAgentPeerCacheLimit {
		oldest := ""
		for token, entry := range c.entries {
			if entry.inFlight {
				continue
			}
			if oldest == "" || entry.at.Before(c.entries[oldest].at) {
				oldest = token
			}
		}
		if oldest != "" {
			delete(c.entries, oldest)
		}
	}
	entry := &threadAgentPeerEntry{spawn: spawn}
	c.entries[spawn.Token] = entry
	return entry
}

// threadAgentPeerAnswer is one agent_rows answer: the rows, or why there
// are none.
type threadAgentPeerAnswer struct {
	listing ThreadAgentRows
	err     error
}

// cachedPeerAgentRows is the last answer for a remote spawn, and whether
// one has arrived.
func (a *App) cachedPeerAgentRows(spawn store.ThreadRequest) (threadAgentPeerAnswer, bool) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[spawn.Token]
	if !ok || !entry.answered {
		return threadAgentPeerAnswer{}, false
	}
	return threadAgentPeerAnswer{listing: entry.listing, err: entry.err}, true
}

// cachedPeerSpawnFor finds the remote spawn whose last answer lists token.
func (a *App) cachedPeerSpawnFor(token string) (store.ThreadRequest, bool) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		for _, row := range entry.listing.Rows {
			if row.Token == token {
				return entry.spawn, true
			}
		}
	}
	return store.ThreadRequest{}, false
}

// refreshPeerAgentRows asks the spawn's computer for its rows in the
// background, unless a call is in flight or an answer is newer than
// minAge.
func (a *App) refreshPeerAgentRows(spawn store.ThreadRequest, minAge time.Duration) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	entry := c.entryLocked(spawn)
	if entry.inFlight || (entry.answered && time.Since(entry.at) < minAge) || a.lifeCtx().Err() != nil {
		c.mu.Unlock()
		return
	}
	entry.inFlight = true
	if c.slots == nil {
		c.slots = make(chan struct{}, threadAgentPeerListsInFlight)
	}
	slots := c.slots
	c.mu.Unlock()
	a.threadRequestsWG.Add(1)
	go func() {
		defer a.threadRequestsWG.Done()
		select {
		case slots <- struct{}{}:
		case <-a.lifeCtx().Done():
			a.abandonPeerAgentRows(spawn.Token)
			return
		}
		defer func() { <-slots }()
		listing, err := a.peerThreadAgentRows(a.lifeCtx(), spawn.TargetComputerID, spawn.CallerThreadID,
			spawn.TargetThreadID, threadAgentRowDepth-1)
		a.notePeerAgentRows(spawn, listing, err)
	}()
}

func (a *App) abandonPeerAgentRows(token string) {
	c := &a.threadAgentPeers
	c.mu.Lock()
	if entry, ok := c.entries[token]; ok {
		entry.inFlight = false
	}
	c.mu.Unlock()
}

// notePeerAgentRows records one agent_rows answer for a remote spawn: the
// cache, the row's nested_open, and a frame for the caller when the
// answer changed what its pane shows.
func (a *App) notePeerAgentRows(spawn store.ThreadRequest, listing ThreadAgentRows, err error) {
	if errors.Is(err, context.Canceled) && a.lifeCtx().Err() != nil {
		a.abandonPeerAgentRows(spawn.Token)
		return
	}
	if err == nil {
		// What the computer lists is open work while any row there has not
		// settled; a computer that did not answer leaves the last record.
		// It is recorded before the rows are, so a read that nests them
		// finds the spawn kept for them.
		open := false
		for _, row := range listing.Rows {
			open = open || row.SettledAt == 0
		}
		if _, setErr := a.store.SetThreadRequestNestedOpen(spawn.Token, open); setErr != nil {
			log.Printf("agent rows: record open work under %s: %v", spawn.Token, setErr)
		}
	}
	c := &a.threadAgentPeers
	c.mu.Lock()
	entry := c.entryLocked(spawn)
	changed := !entry.answered || errorText(entry.err) != errorText(err) ||
		(err == nil && !reflect.DeepEqual(entry.listing, listing))
	entry.answered, entry.at, entry.inFlight, entry.err = true, time.Now(), false, err
	if err == nil {
		entry.listing = listing
	}
	c.mu.Unlock()
	if changed {
		a.noteThreadRequestChanged(store.ThreadRequestChange{CallerThreadID: spawn.CallerThreadID})
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// refreshNestedOpenRemoteSpawns asks again about every settled remote spawn
// whose computer last listed open work, at the poll's pace, until it lists
// none.
func (a *App) refreshNestedOpenRemoteSpawns() {
	if a.backends == nil {
		return
	}
	c := &a.threadAgentPeers
	c.mu.Lock()
	after := c.sweepAfter
	c.mu.Unlock()
	rows, err := a.store.ListNestedOpenRemoteSpawns(after.SettledAt, after.Token, threadRequestPollBatch)
	if err != nil {
		logThreadRequestSweep("read remote spawns with open work", err)
		return
	}
	next := store.ThreadRequest{}
	if len(rows) == threadRequestPollBatch {
		next = rows[len(rows)-1]
	}
	c.mu.Lock()
	c.sweepAfter = store.ThreadRequest{SettledAt: next.SettledAt, Token: next.Token}
	c.mu.Unlock()
	for _, row := range rows {
		a.refreshPeerAgentRows(row, a.threadPollNormal())
	}
}

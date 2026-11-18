package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
)

// Recovery streams the peer's current work in bounded pages under one launch.
// The preceding wait covers activity that changes while those pages are consumed.

// OwnerPageKey is the last stable key consumed in one recovery phase.
type OwnerPageKey struct {
	// Phase selects prompts, active work, uncollected answers or structural changes.
	Phase string `json:"phase,omitempty"`

	// After is the exclusive lower bound of the page.
	After string `json:"after,omitempty"`

	// Through bounds the current group of active threads while receipts are paged.
	Through string `json:"through,omitempty"`

	// Token is the last receipt in that active group.
	Token string `json:"token,omitempty"`
}

// OwnerRecoveryPage is one bounded part of the authenticated device's recovery listing.
type OwnerRecoveryPage struct {
	// Tokens need current request or link state.
	Tokens []string `json:"tokens,omitempty"`

	// Structural contains the current facts for pending edits.
	Structural []store.SpawnRevision `json:"structural,omitempty"`

	// Next continues the listing, or is absent after the final page.
	Next *OwnerPageKey `json:"next,omitempty"`
}

const ownerPageSize = 32

func (a *App) ownerLaunch(launch string) error {
	server := a.transportServer.Load()
	if server == nil || launch == "" || launch != server.LaunchID() {
		return fmt.Errorf("the peer launch changed; reconcile its work")
	}
	return nil
}

// ThreadOwnerRecovery lists the current sources of work for the authenticated
// owner. The version in the preceding wait covers every page of the listing.
//
//ao:scope threads:read
//ao:route selected
//ao:callerbound
func (a *App) ThreadOwnerRecovery(ctx context.Context, launch string, key OwnerPageKey) (OwnerRecoveryPage, error) {
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return OwnerRecoveryPage{}, err
	}
	if owner == threadReceiptLocalOwner {
		return OwnerRecoveryPage{}, fmt.Errorf("owner recovery requires a paired connection")
	}
	if err = a.ownerLaunch(launch); err != nil {
		return OwnerRecoveryPage{}, err
	}
	a.threadRequests.mu.Lock()
	barred := a.threadRequests.rest.barred
	a.threadRequests.mu.Unlock()
	if barred {
		return OwnerRecoveryPage{}, fmt.Errorf("worker recovery is in progress")
	}
	var page OwnerRecoveryPage
	switch key.Phase {
	case "", "prompts":
		tokens := a.workerPrompts.promptSpawns(owner, key.After)
		if len(tokens) > ownerPageSize {
			page.Tokens = tokens[:ownerPageSize]
			page.Next = &OwnerPageKey{Phase: "prompts", After: page.Tokens[ownerPageSize-1]}
		} else {
			page.Tokens = tokens
			page.Next = &OwnerPageKey{Phase: "active"}
		}
	case "active":
		live := a.liveWorkerThreads()
		ids, more, err := a.store.ThreadsWithOpenAgentRows(a.parkedRequestTokens(), live, key.After, ownerPageSize)
		if err != nil {
			return page, err
		}
		for _, id := range live {
			if id > key.After {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		if len(ids) > ownerPageSize {
			more = true
			ids = ids[:ownerPageSize]
		}
		through := key.Through
		if through != "" {
			ids = slices.DeleteFunc(ids, func(id string) bool { return id > through })
		} else if len(ids) > 0 {
			through = ids[len(ids)-1]
		}
		page.Tokens, err = a.store.RecoveryOwnerTokens(owner, ids, key.Token, ownerPageSize+1)
		if err != nil {
			return page, err
		}
		switch {
		case len(page.Tokens) > ownerPageSize:
			page.Tokens = page.Tokens[:ownerPageSize]
			page.Next = &OwnerPageKey{Phase: "active", After: key.After, Through: through, Token: page.Tokens[ownerPageSize-1]}
		case more || key.Through != "":
			page.Next = &OwnerPageKey{Phase: "active", After: through}
		default:
			page.Next = &OwnerPageKey{Phase: "answers"}
		}
	case "answers":
		page.Tokens, err = a.store.UncollectedOwnerTokens(owner, key.After, ownerPageSize)
		if err != nil {
			return page, err
		}
		page.Next = &OwnerPageKey{Phase: "structural"}
		if len(page.Tokens) == ownerPageSize {
			page.Next = &OwnerPageKey{Phase: "answers", After: page.Tokens[len(page.Tokens)-1]}
		}
	case "structural":
		page.Structural, err = a.store.PendingSpawnRevisions(owner, key.After, ownerPageSize)
		if err != nil {
			return page, err
		}
		if len(page.Structural) == ownerPageSize {
			page.Next = &OwnerPageKey{Phase: "structural", After: page.Structural[len(page.Structural)-1].Token}
		}
	default:
		return page, fmt.Errorf("unknown recovery phase %q", key.Phase)
	}
	return page, nil
}

func (w *workerPrompts) promptSpawns(owner, after string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, p := range w.byToken {
		if p.state == workerPromptRouted && p.remote != nil && p.remote.device == owner && p.remote.spawnToken > after {
			out = append(out, p.remote.spawnToken)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// OwnerLinkRead authenticates a state read or contact through a spawn receipt.
type OwnerLinkRead struct {
	// Token names the spawn receipt.
	Token string `json:"token"`

	// Source identifies the original spawner on the calling computer.
	Source string `json:"source"`

	// Held lists the prompt tokens whose leases the caller renews.
	Held []string `json:"held,omitempty"`
}

// OwnerLinkState reports the target or the reason its listing is unavailable.
type OwnerLinkState struct {
	// Token identifies the requested spawn.
	Token string `json:"token"`

	// End describes pending creation, release, detachment or deletion.
	End string `json:"end,omitempty"`

	// State holds the current shallow listing when the target survives.
	State *ThreadAgentRows `json:"state,omitempty"`
}

func (a *App) ownerLinkStates(ctx context.Context, owner string, links []OwnerLinkRead) ([]OwnerLinkState, error) {
	var results []OwnerLinkState
	for _, link := range links {
		reach, found, err := a.store.ReachSpawnLink(link.Token, owner, link.Source, time.Now().UnixMilli())
		if err != nil {
			return nil, err
		}
		result := OwnerLinkState{Token: link.Token}
		switch {
		case !found:
			result.End = "gone"
		case reach.TargetThreadID == "":
			if reach.Open() {
				result.End = "pending"
			} else {
				result.End = "detached"
			}
		default:
			state, err := a.answerThreadAgentRows(ctx, owner, link.Source, threadAgentPeerArgs{ThreadID: reach.TargetThreadID, Spawn: link.Token, StateOnly: true, Depth: 1, Held: link.Held})
			if err != nil {
				return nil, err
			}
			result.State = &state
			if !reach.Linked {
				result.End = "released"
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func (a *App) readOwnerWork(ctx context.Context, owner string, poll ThreadPeerPoll) ([]OwnerLinkState, error) {
	if err := a.ownerLaunch(poll.Launch); err != nil {
		return nil, err
	}
	if owner == threadReceiptLocalOwner || len(poll.Links) > threadPeerTokenLimit || len(poll.Contacts) > threadPeerTokenLimit || len(poll.StructuralAck) > threadPeerTokenLimit {
		return nil, fmt.Errorf("invalid owner read")
	}
	if err := a.store.AcknowledgeSpawnRevisions(owner, poll.StructuralAck); err != nil {
		return nil, err
	}
	for _, link := range poll.Contacts {
		if _, _, err := a.store.ReachSpawnLink(link.Token, owner, link.Source, time.Now().UnixMilli()); err != nil {
			return nil, err
		}
	}
	return a.ownerLinkStates(ctx, owner, poll.Links)
}

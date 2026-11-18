package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// An owner batch applies request status and link state from one peer observation.
// Prompt deliveries complete before the batch acknowledges its structural edits.
func (a *App) ownerPoll(launch string, rows []store.ThreadRequest) (ThreadPeerPoll, map[string]store.ThreadRequest) {
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
	return poll, spawns
}

func (a *App) applyOwnerStatus(id string, answer ThreadPeerRequest, started time.Time) ([]threadWakeRef, error) {
	row, found, err := a.store.GetThreadRequest(answer.Token)
	if err != nil || !found || row.TargetComputerID != id {
		return nil, err
	}
	// Only the dispatch clock can rule out an admission still in flight.
	if !answer.Known && row.State == store.ThreadRequestUnconfirmed {
		return nil, nil
	}
	wakes, err := a.applyKnownPeerRow(row, answer, threadPeerHearing{at: started, v2: true})
	if err != nil || !answer.Known || !store.ThreadReceiptSettled(answer.State) {
		return wakes, err
	}
	current, found, err := a.store.GetThreadRequest(answer.Token)
	if err == nil && found && !threadLateReplyAwaited(current, started) {
		_, err = a.store.RetireThreadRequestPoll(answer.Token)
	}
	return wakes, err
}

func (a *App) applyOwnerLink(read ownerRead, origin attachedbackends.PeerRead, spawn store.ThreadRequest, state OwnerLinkState, end string) (<-chan error, error) {
	id := spawn.TargetComputerID
	if state.End == "pending" {
		return nil, nil
	}

	if spawn.TargetThreadID == "" {
		current, found, err := a.store.GetThreadRequest(state.Token)
		if err != nil {
			return nil, err
		}
		if !found || current.TargetThreadID == "" {
			return nil, nil
		}
		spawn = current
	}
	if state.State == nil && (end == "gone" || end == "detached") {
		state.End = end
	}
	revision := store.SpawnRevision{SpawnRevisionAck: store.SpawnRevisionAck{Token: state.Token}, End: state.End}
	if state.State != nil {
		revision.Title = state.State.Title
	}
	if _, err := a.store.ApplySpawnRevision(id, revision); err != nil {
		return nil, err
	}
	listing := ThreadAgentRows{}
	if state.State != nil {
		listing = *state.State
	}
	if _, err := a.store.SetThreadRequestNestedOpen(state.Token, peerListingOpen(listing)); err != nil {
		return nil, err
	}
	done := a.noteRemotePrompts(read.delivery(), origin, spawn, listing, nil)
	c := &a.threadAgentPeers
	c.mu.Lock()
	changed := c.noteAnswerLocked(spawn, listing, nil, true)
	c.mu.Unlock()
	if changed {
		a.projectPeerRows(spawn.CallerThreadID)
	}
	return done, nil
}
func (a *App) readOwnerBatch(ctx context.Context, id, launch string, keys []string, structural []store.SpawnRevision) error {
	read := a.ownerReads.start(ctx, id, keys)
	defer read.close()
	rows, err := a.store.OwnerRequests(id, keys)
	if err != nil {
		return err
	}
	poll, spawns := a.ownerPoll(launch, rows)
	call, cancel := context.WithTimeout(ctx, threadPollCallTimeout)
	defer cancel()
	if len(rows) == 0 {
		for _, edit := range structural {
			if slices.Contains(keys, edit.Token) {
				poll.StructuralAck = append(poll.StructuralAck, edit.SpawnRevisionAck)
			}
		}
		return a.ackOwnerEdits(call, id, launch, poll.StructuralAck)
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
	err = read.commit(origin, launch, nil, func() error {
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
			w, err := a.applyOwnerStatus(id, answer, started)
			wakes = append(wakes, w...)
			if err != nil {
				return err
			}
		}
		for i, state := range reply.Links {
			if state.Token != poll.Links[i].Token {
				return fmt.Errorf("unexpected link token %q", state.Token)
			}
			done, err := a.applyOwnerLink(read, origin, spawns[state.Token], state, ends[state.Token])
			if err != nil {
				return err
			}
			if done != nil {
				promptResults = append(promptResults, done)
			}
		}
		return nil
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
	return a.ackOwnerEdits(call, id, launch, acks)
}

func (a *App) ackOwnerEdits(ctx context.Context, id, launch string, acks []store.SpawnRevisionAck) error {
	if len(acks) == 0 {
		return nil
	}
	call, cancel := context.WithTimeout(ctx, threadPollCallTimeout)
	defer cancel()
	return a.backends.CallThreadPeerOn(call, id, transport.CapabilityThreadToolsV2, "ThreadToolRequestStatus", &ThreadPeerPollReply{}, ThreadPeerPoll{Launch: launch, StructuralAck: acks})
}

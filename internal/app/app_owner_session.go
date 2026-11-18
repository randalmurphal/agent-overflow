package app

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// A session receives changes independently of its two readers. An observation
// stays owed until a reader applies that generation, including across reconnects.
type ownerObligation struct {
	generation uint64
	prompt     bool
	edit       store.SpawnRevision
}

type ownerSession struct {
	scope             ownerRead
	app               *App
	peer              *ownerPeer
	id                string
	launch            string
	ordinary, prompts chan struct{}
	generation        uint64
}

func (a *App) waitOwnerChanges(ctx context.Context, id string, cursor OwnerCursor) (OwnerChanges, attachedbackends.PeerRead, error) {
	call, cancel := context.WithTimeout(ctx, threadPeerCallWait("ThreadOwnerChanges"))
	defer cancel()
	if hook := a.beforeOwnerWaitForTest; hook != nil {
		hook(call, cursor)
	}
	var changes OwnerChanges
	origin, err := a.backends.ReadThreadPeer(call, id, transport.CapabilityThreadToolsV2, "", "ThreadOwnerChanges", &changes, cursor)
	return changes, origin, err
}

func (a *App) runOwnerSession(ctx context.Context, id string, p *ownerPeer) error {
	read := a.ownerReads.start(ctx, id, nil)
	first, origin, err := a.waitOwnerChanges(ctx, id, OwnerCursor{})
	if err != nil {
		if !attachedbackends.Unsupported(err) || p.launch != "" {
			a.invalidateOwnerRead(read, err)
		}
		return err
	}
	if !first.Reset || first.Launch == "" {
		return errors.New("peer omitted reset on first owner wait")
	}
	ctx, cancel := context.WithCancelCause(origin.Bind(ctx))
	s := &ownerSession{app: a, peer: p, id: id, launch: first.Launch, ordinary: make(chan struct{}, 1), prompts: make(chan struct{}, 1)}
	err = read.commit(origin, first.Launch, nil, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.invalidateOwnerTreesLocked(id, nil)
		s.scope = ownerRead{owner: &a.ownerReads, generation: a.ownerReads.generationLocked(id)}
		a.ownerUpdates.mu.Lock()
		defer a.ownerUpdates.mu.Unlock()
		p.origin = origin
		p.ready, p.legacy, p.launch, p.disconnected = false, false, first.Launch, origin.Disconnected()
		if p.owed == nil {
			p.owed = make(map[string]ownerObligation)
		}
		for token, previous := range p.owed {
			p.owed[token] = ownerObligation{prompt: previous.prompt}
		}
		return nil
	})
	if err != nil {
		cancel(err)
		return err
	}
	ctx = s.scope.bind(ctx)
	var workers sync.WaitGroup
	for _, prompt := range []bool{true, false} {
		workers.Go(func() {
			if !prompt {
				if err := a.recoverOwnerPeer(ctx, id, first.Launch); err != nil {
					cancel(err)
					return
				}
				if err := s.scope.commit(origin, first.Launch, nil, func() error {
					a.ownerUpdates.mu.Lock()
					defer a.ownerUpdates.mu.Unlock()
					if ctx.Err() != nil {
						return ctx.Err()
					}
					p.ready = true
					return nil
				}); err != nil {
					cancel(err)
					return
				}
			}
			if err := s.read(ctx, prompt); err != nil {
				cancel(err)
			}
		})
	}
	defer func() {
		cancel(context.Canceled)
		a.invalidateOwnerRead(s.scope, context.Cause(ctx))
		workers.Wait()
	}()
	cursor := first.OwnerCursor
	for ctx.Err() == nil {
		answer, from, err := a.waitOwnerChanges(ctx, id, cursor)
		if err != nil {
			cancel(err)
			break
		}
		if err := s.observe(ctx, answer, from); err != nil {
			cancel(err)
			break
		}
		cursor = answer.OwnerCursor
	}
	return context.Cause(ctx)
}

func (s *ownerSession) observe(ctx context.Context, answer OwnerChanges, from attachedbackends.PeerRead) error {
	var obsolete []store.SpawnRevisionAck
	err := s.scope.commit(from, s.launch, nil, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if answer.Reset || answer.Launch != s.launch {
			s.app.invalidateOwnerTreesLocked(s.id, nil)
			return errOwnerCursorReset
		}
		var err error
		obsolete, err = s.record(answer, from)
		return err
	})
	if err != nil {
		return err
	}
	return s.app.ackOwnerEdits(ctx, s.id, s.launch, obsolete)
}

var errOwnerCursorReset = errors.New("peer activity cursor reset")

func signalOwner(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// record admits only this computer's local requests, including lineage-only spawns.
// A receipt or edit on the destination can outlive its row here.
func (s *ownerSession) record(answer OwnerChanges, origin attachedbackends.PeerRead) ([]store.SpawnRevisionAck, error) {
	changes := make(map[string]ownerObligation, len(answer.Tokens)+len(answer.Structural))
	for _, key := range answer.Tokens {
		changes[key] = ownerObligation{}
	}
	for _, key := range answer.PromptTokens {
		changes[key] = ownerObligation{prompt: true}
	}
	for _, edit := range answer.Structural {
		entry := changes[edit.Token]
		entry.edit = edit
		changes[edit.Token] = entry
	}
	keys, err := s.app.store.OwnerRequestTokens(s.id, slices.Collect(maps.Keys(changes)))
	if err != nil {
		return nil, err
	}
	s.app.ownerReads.changedLocked(keys)
	s.app.ownerUpdates.mu.Lock()
	defer s.app.ownerUpdates.mu.Unlock()
	s.peer.disconnected = origin.Disconnected()
	s.generation++
	for _, key := range keys {
		entry := s.peer.owed[key]
		change := changes[key]
		entry.generation = s.generation
		entry.prompt = entry.prompt || change.prompt
		if change.edit.Revision != 0 {
			entry.edit = change.edit
		}
		s.peer.owed[key] = entry
		delete(changes, key)
	}
	var obsolete []store.SpawnRevisionAck
	for key, change := range changes {
		delete(s.peer.owed, key)
		if change.edit.Revision != 0 {
			obsolete = append(obsolete, change.edit.SpawnRevisionAck)
		}
	}
	signalOwner(s.prompts)
	signalOwner(s.ordinary)
	return obsolete, nil
}

func (s *ownerSession) read(ctx context.Context, prompt bool) error {
	wake := s.ordinary
	if prompt {
		wake = s.prompts
	}
	for ctx.Err() == nil {
		s.app.ownerUpdates.mu.Lock()
		var keys []string
		var edits []store.SpawnRevision
		batch := make(map[string]ownerObligation)
		for key, entry := range s.peer.owed {
			if entry.prompt != prompt {
				continue
			}
			keys = append(keys, key)
			if entry.edit.Revision != 0 {
				edits = append(edits, entry.edit)
			}
			batch[key] = entry
			if len(keys) == ownerPageSize {
				break
			}
		}
		s.app.ownerUpdates.mu.Unlock()
		if len(keys) == 0 {
			select {
			case <-ctx.Done():
			case <-wake:
			}
			continue
		}
		if err := s.app.readOwnerChanges(ctx, s.id, s.launch, keys, edits); err != nil {
			return err
		}
		if err := s.scope.commit(s.peer.origin, s.launch, nil, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			s.app.invalidateChangedOwnerTrees(s.id, keys)
			s.app.ownerUpdates.mu.Lock()
			for key, entry := range batch {
				if s.peer.owed[key] == entry {
					delete(s.peer.owed, key)
				}
			}
			s.app.ownerUpdates.mu.Unlock()
			return nil
		}); err != nil {
			return err
		}
	}
	return ctx.Err()
}

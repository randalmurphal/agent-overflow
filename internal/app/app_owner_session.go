package app

import (
	"context"
	"errors"
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
	app               *App
	peer              *ownerPeer
	id                string
	launch            string
	ordinary, prompts chan struct{}
	generation        uint64
	// progress and resume are guarded with the peer's debt under ownerUpdates.mu.
	progress chan struct{}
	resume   context.CancelFunc
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
	first, origin, err := a.waitOwnerChanges(ctx, id, OwnerCursor{})
	if err != nil {
		return err
	}
	if !first.Reset {
		return errors.New("peer omitted reset on first owner wait")
	}
	ctx, cancel := context.WithCancelCause(origin.Bind(ctx))
	s := &ownerSession{app: a, peer: p, id: id, launch: first.Launch, ordinary: make(chan struct{}, 1), prompts: make(chan struct{}, 1)}
	err = origin.Apply(first.Launch, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.ownerUpdates.mu.Lock()
		defer a.ownerUpdates.mu.Unlock()
		s.progress = make(chan struct{})
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
	a.invalidateOwnerTrees(id, nil)
	var workers sync.WaitGroup
	for _, prompt := range []bool{true, false} {
		workers.Go(func() {
			if !prompt {
				if err := a.recoverOwnerPeer(ctx, id, first.Launch); err != nil {
					cancel(err)
					return
				}
				if err := origin.Apply(first.Launch, func() error {
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
		a.ownerUpdates.mu.Lock()
		p.ready = false
		a.ownerUpdates.mu.Unlock()
		workers.Wait()
	}()
	cursor := first.OwnerCursor
	for ctx.Err() == nil {
		answer, from, err := a.waitOwnerChanges(ctx, id, cursor)
		if err != nil {
			cancel(err)
			break
		}
		if answer.Reset {
			cancel(errOwnerCursorReset)
			break
		}
		if err := s.observe(ctx, answer, from); err != nil {
			cancel(err)
			break
		}
		cursor = answer.OwnerCursor
		if s.full() {
			if err := s.waitForRoom(ctx, cursor); err != nil {
				cancel(err)
				break
			}
		}
	}
	return context.Cause(ctx)
}

// While normal reads have filled the queue, B retains their cursor. The priority
// cursor can advance independently until the ordinary reader makes room.
func (s *ownerSession) waitForRoom(ctx context.Context, cursor OwnerCursor) error {
	hints, stop := context.WithCancel(ctx)
	defer stop()
	s.app.ownerUpdates.mu.Lock()
	if len(s.peer.owed) < ownerChangeLimit {
		s.app.ownerUpdates.mu.Unlock()
		return nil
	}
	s.resume = stop
	s.app.ownerUpdates.mu.Unlock()
	defer func() { s.app.ownerUpdates.mu.Lock(); s.resume = nil; s.app.ownerUpdates.mu.Unlock() }()
	for hints.Err() == nil {
		cursor.PromptsOnly = true
		answer, from, err := s.app.waitOwnerChanges(hints, s.id, cursor)
		if hints.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if answer.Reset {
			return errOwnerCursorReset
		}
		if err = s.observe(ctx, answer, from); err != nil {
			return err
		}
		cursor = answer.OwnerCursor
		// Only one extra priority page is admitted under backpressure.
		if err = s.waitPriorityPage(hints, answer.PromptTokens); err != nil {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (s *ownerSession) waitPriorityPage(ctx context.Context, keys []string) error {
	for len(keys) > 0 {
		s.app.ownerUpdates.mu.Lock()
		keys = slices.DeleteFunc(keys, func(key string) bool { return !s.peer.owed[key].prompt })
		room := s.progress
		s.app.ownerUpdates.mu.Unlock()
		if len(keys) > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-room:
			}
		}
	}
	return nil
}

func (s *ownerSession) observe(ctx context.Context, answer OwnerChanges, from attachedbackends.PeerRead) error {
	keys := append(slices.Clone(answer.Tokens), ownerRevisionTokens(answer.Structural)...)
	s.app.ownerReads.changed(keys)
	return from.Apply(answer.Launch, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.record(answer, from, keys)
		return nil
	})
}

var errOwnerCursorReset = errors.New("peer activity cursor reset")

func signalOwner(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *ownerSession) full() bool {
	s.app.ownerUpdates.mu.Lock()
	defer s.app.ownerUpdates.mu.Unlock()
	return len(s.peer.owed) >= ownerChangeLimit
}

func (s *ownerSession) record(answer OwnerChanges, origin attachedbackends.PeerRead, keys []string) {
	s.app.ownerUpdates.mu.Lock()
	defer s.app.ownerUpdates.mu.Unlock()
	s.peer.disconnected = origin.Disconnected()
	s.generation++
	for _, token := range keys {
		entry := s.peer.owed[token]
		entry.generation = s.generation
		s.peer.owed[token] = entry
	}
	for _, token := range answer.PromptTokens {
		entry := s.peer.owed[token]
		entry.prompt = true
		s.peer.owed[token] = entry
	}
	for _, edit := range answer.Structural {
		entry := s.peer.owed[edit.Token]
		entry.edit = edit
		s.peer.owed[edit.Token] = entry
	}
	signalOwner(s.prompts)
	signalOwner(s.ordinary)
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
		s.app.invalidateChangedOwnerTrees(s.id, keys)
		s.app.ownerUpdates.mu.Lock()
		for key, entry := range batch {
			if s.peer.owed[key] == entry {
				delete(s.peer.owed, key)
			}
		}
		close(s.progress)
		s.progress = make(chan struct{})
		if s.resume != nil && len(s.peer.owed) < ownerChangeLimit {
			s.resume()
		}
		s.app.ownerUpdates.mu.Unlock()
	}
	return ctx.Err()
}

package app

import (
	"context"
	"errors"
	"sync"

	"agent-overflow/internal/attachedbackends"
)

// Owner reads order overlapping observations. Completing an older read cannot undo a newer one.
var errReadSuperseded = errors.New("a newer peer observation superseded this read")

// Only in-flight reads need ordering. An observation after a read began or
// a later read already applied prevents that read from committing.
type ownerReads struct {
	mu     sync.Mutex
	next   uint64
	active map[string]*ownerReadOrder
	peers  map[string]*peerReadGeneration
}

type ownerReadOrder struct {
	count   int
	barrier uint64
}

// All observations on one computer share a generation until reset, disconnect or forget.
type peerReadGeneration struct{ computer string }
type peerGenerationKey struct{}
type ownerRead struct {
	generation *peerReadGeneration
	owner      *ownerReads
	keys       []string
	order      uint64
}

func (o *ownerReads) start(ctx context.Context, computer string, keys []string) ownerRead {
	o.mu.Lock()
	defer o.mu.Unlock()
	generation, _ := ctx.Value(peerGenerationKey{}).(*peerReadGeneration)
	if generation == nil || generation.computer != computer {
		generation = o.generationLocked(computer)
	}
	if o.active == nil {
		o.active = map[string]*ownerReadOrder{}
	}
	o.next++
	for _, key := range keys {
		entry := o.active[key]
		if entry == nil {
			entry = &ownerReadOrder{}
			o.active[key] = entry
		}
		entry.count++
	}
	return ownerRead{owner: o, generation: generation, keys: keys, order: o.next}
}

func (o *ownerReads) generationLocked(id string) *peerReadGeneration {
	if o.peers == nil {
		o.peers = map[string]*peerReadGeneration{}
	}
	if o.peers[id] == nil {
		o.peers[id] = &peerReadGeneration{computer: id}
	}
	return o.peers[id]
}

func (r ownerRead) bind(ctx context.Context) context.Context {
	return context.WithValue(ctx, peerGenerationKey{}, r.generation)
}

// The prompt queue preserves the order of accepted batches. A queued delivery
// still belongs to that batch's peer generation after its token read has closed.
func (r ownerRead) delivery() ownerRead { return ownerRead{owner: r.owner, generation: r.generation} }

var errPeerReadInvalidated = errors.New("the peer read generation ended")

// commit validates the peer generation, token order and responding carrier together.
// A local call failure has no responding carrier and can only record that failure.
func (r ownerRead) commit(origin attachedbackends.PeerRead, launch string, callErr error, record func() error) error {
	return r.apply(func() error {
		if callErr != nil && !origin.Answered() {
			return record()
		}
		if err := origin.Validate(launch); err != nil {
			return err
		}
		return record()
	})
}

func (o *ownerReads) changed(keys []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.changedLocked(keys)
}
func (o *ownerReads) changedLocked(keys []string) {
	o.next++
	for _, key := range keys {
		if e := o.active[key]; e != nil {
			e.barrier = o.next
		}
	}
}

func (r ownerRead) apply(commit func() error) error {
	o := r.owner
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.peers[r.generation.computer] != r.generation {
		return errPeerReadInvalidated
	}
	for _, key := range r.keys {
		if e := o.active[key]; e == nil || e.barrier > r.order {
			return errReadSuperseded
		}
	}
	// A partial apply also bars older answers. The failed read remains owed.
	for _, key := range r.keys {
		o.active[key].barrier = r.order
	}
	return commit()
}

func (r ownerRead) close() {
	o := r.owner
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, key := range r.keys {
		e := o.active[key]
		e.count--
		if e.count == 0 {
			delete(o.active, key)
		}
	}
}

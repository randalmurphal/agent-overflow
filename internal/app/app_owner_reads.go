package app

import (
	"errors"
	"sync"
)

// Owner reads order overlapping observations. Completing an older read cannot undo a newer one.
var errReadSuperseded = errors.New("a newer peer observation superseded this read")

// Only in-flight reads need ordering. An observation after a read began or
// a later read already applied prevents that read from committing.
type ownerReads struct {
	mu     sync.Mutex
	next   uint64
	active map[string]*ownerReadOrder
}

type ownerReadOrder struct {
	count   int
	barrier uint64
}

type ownerRead struct {
	owner *ownerReads
	keys  []string
	order uint64
}

func (o *ownerReads) start(keys []string) ownerRead {
	o.mu.Lock()
	defer o.mu.Unlock()
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
	return ownerRead{owner: o, keys: keys, order: o.next}
}

func (o *ownerReads) changed(keys []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
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

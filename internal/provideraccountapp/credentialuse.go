package provideraccountapp

import (
	"context"
	"errors"
	"sync"
)

// ErrSelectionMoved refuses a credential use whose selection generation is no
// longer the provider's: the account it was chosen for is not the one in the
// canonical home any more.
var ErrSelectionMoved = errors.New("the provider account changed after the work was chosen for it")

// credentialUses is the registry of provider processes that run against a
// provider's canonical credential for their whole life (a text-generation
// CLI), so that a path replacing that credential can end them first and none
// can start while it runs. Its mutex is a leaf: nothing else is locked under
// it, and nobody waits on a channel or a context while holding it.
type credentialUses struct {
	mu sync.Mutex
	// running holds each provider's live uses.
	running map[string]map[*credentialUse]struct{}
	// changing is set while a path is replacing the provider's canonical
	// credential (lockCredentialChange), and is closed when the change ends.
	changing map[string]chan struct{}
}

type credentialUse struct {
	end  func()
	done chan struct{}
	once sync.Once
}

// BeginCredentialUse admits one process that runs against providerName's
// canonical credential under the selection generation it was chosen for.
// While a path is replacing that credential it waits for the change to end
// (or for ctx to end), then admits only if generation is still the
// provider's; otherwise it returns ErrSelectionMoved. end stops the process;
// a path replacing the credential calls it and waits for release, which the
// caller calls once the process has exited, or once it failed to start.
func (m *Manager) BeginCredentialUse(
	ctx context.Context,
	providerName string,
	generation uint64,
	end func(),
) (release func(), err error) {
	use := &credentialUse{end: end, done: make(chan struct{})}
	for {
		m.uses.mu.Lock()
		changing := m.uses.changing[providerName]
		if changing == nil {
			if m.uses.running == nil {
				m.uses.running = make(map[string]map[*credentialUse]struct{})
			}
			if m.uses.running[providerName] == nil {
				m.uses.running[providerName] = make(map[*credentialUse]struct{})
			}
			m.uses.running[providerName][use] = struct{}{}
			m.uses.mu.Unlock()
			break
		}
		m.uses.mu.Unlock()
		select {
		case <-changing:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	release = func() {
		use.once.Do(func() {
			m.uses.mu.Lock()
			delete(m.uses.running[providerName], use)
			m.uses.mu.Unlock()
			close(use.done)
		})
	}
	// Registered first and verified after, so a path that starts replacing
	// the credential after the registration ends this use, and one that
	// finished before it has moved the generation read here.
	if m.Generation(providerName) != generation {
		release()
		return nil, ErrSelectionMoved
	}
	return release, nil
}

// lockCredentialChange takes providerName's reconcile lock for a path that
// may replace the provider's canonical credential, and returns with no
// credential use alive: it ends every running use and waits for each to
// release. Until the returned end runs, no use is admitted; then waiting uses
// read the generation again. end releases the reconcile lock as well, so the
// change lasts exactly as long as the lock: a path ends it where it releases
// the lock early and defers it for every other return. end is idempotent.
//
// The wait is for the processes to exit; their stop is a signal and then a
// kill (textgen's ExecCLI), so it is bounded by that.
func (m *Manager) lockCredentialChange(providerName string) (end func()) {
	reconcileMu := m.reconcileMutex(providerName)
	reconcileMu.Lock()
	changing := make(chan struct{})
	m.uses.mu.Lock()
	if m.uses.changing == nil {
		m.uses.changing = make(map[string]chan struct{})
	}
	// Only a holder of the reconcile lock sets the mark, and it clears the
	// mark before releasing the lock, so no earlier mark is set here.
	m.uses.changing[providerName] = changing
	live := make([]*credentialUse, 0, len(m.uses.running[providerName]))
	for use := range m.uses.running[providerName] {
		live = append(live, use)
	}
	m.uses.mu.Unlock()
	for _, use := range live {
		use.end()
	}
	for _, use := range live {
		<-use.done
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.uses.mu.Lock()
			delete(m.uses.changing, providerName)
			m.uses.mu.Unlock()
			close(changing)
			reconcileMu.Unlock()
		})
	}
}

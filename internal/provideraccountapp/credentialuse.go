package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"agent-overflow/internal/provider"
)

// ErrSelectionMoved refuses a credential use whose selection generation is no
// longer the provider's: the account it was chosen for is not the one in the
// canonical home any more.
var ErrSelectionMoved = errors.New("the provider account changed after the work was chosen for it")

// credentialUses is one provider's registry of processes that run against its
// canonical credential for their whole life (a text-generation CLI). It lives
// in the provider's reconcile lock. A holder of that lock about to change the
// selected account ends every use and waits for it to exit
// (Manager.selectionChanging), and none is admitted until the lock is
// released. Its mutex is a leaf: nothing else is locked under it, and nobody
// waits on a channel or a context while holding it.
type credentialUses struct {
	mu sync.Mutex
	// running holds the live uses.
	running map[*credentialUse]struct{}
	// changing is set while a holder of the reconcile lock changes the
	// selection, and closed when that lock is released.
	changing chan struct{}
}

type credentialUse struct {
	end  func()
	done chan struct{}
	once sync.Once
}

// uses is providerName's credential-use registry, or an error for a provider
// that has none.
func (m *Manager) uses(providerName string) (*credentialUses, error) {
	switch providerName {
	case string(provider.Claude), string(provider.Codex), string(provider.Agy):
		return &m.reconcileMutex(providerName).uses, nil
	default:
		return nil, fmt.Errorf("no provider account selection for %q", providerName)
	}
}

// BeginCredentialUse admits one process that runs against providerName's
// canonical credential under the selection generation it was chosen for.
// While the selection is changing it waits for the change to end (or for ctx
// to end), then admits only if generation is still the provider's; otherwise
// it returns ErrSelectionMoved. end stops the process; a change of the
// selection calls it and waits for release, which the caller calls once the
// process has exited, or once it failed to start.
func (m *Manager) BeginCredentialUse(
	ctx context.Context,
	providerName string,
	generation uint64,
	end func(),
) (release func(), err error) {
	uses, err := m.uses(providerName)
	if err != nil {
		return nil, err
	}
	use := &credentialUse{end: end, done: make(chan struct{})}
	for {
		uses.mu.Lock()
		changing := uses.changing
		if changing == nil {
			if uses.running == nil {
				uses.running = make(map[*credentialUse]struct{})
			}
			uses.running[use] = struct{}{}
			uses.mu.Unlock()
			break
		}
		uses.mu.Unlock()
		select {
		case <-changing:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	release = func() {
		use.once.Do(func() {
			uses.mu.Lock()
			delete(uses.running, use)
			uses.mu.Unlock()
			close(use.done)
		})
	}
	// Registered first and verified after, so a change that starts after the
	// registration ends this use, and one that finished before it has moved
	// the generation read here. Generation takes no Manager lock, so a change
	// holding mu can wait for this use.
	if m.Generation(providerName) != generation {
		release()
		return nil, ErrSelectionMoved
	}
	return release, nil
}

// selectionChanging is the one place a provider's running credential uses
// are ended: every path calls it, holding the provider's reconcile lock,
// once it has decided to change which account is selected, and before it
// reads the outgoing canonical credential or installs another account's. A
// path that refuses or declines the change does not call it. It ends every running use, waits for
// each to exit, and admits none until the reconcile lock is released. A
// second call in the same hold finds the change already marked; no use was
// admitted since. The store refuses a selection change while a use is alive
// (refuseSelectionWithLiveUses), so a path that skips this fails loudly.
//
// mu may be held: a use's admission and release take neither mu nor the
// reconcile lock. The wait is for the processes to exit; their stop is a
// signal and then a kill (textgen's ExecCLI), so it is bounded by that.
func (m *Manager) selectionChanging(providerName string) {
	uses := &m.reconcileMutex(providerName).uses
	uses.mu.Lock()
	if uses.changing != nil {
		uses.mu.Unlock()
		return
	}
	uses.changing = make(chan struct{})
	live := make([]*credentialUse, 0, len(uses.running))
	for use := range uses.running {
		live = append(live, use)
	}
	uses.mu.Unlock()
	for _, use := range live {
		use.end()
	}
	for _, use := range live {
		<-use.done
	}
}

// endChange admits uses again; the reconcile lock calls it as it is released.
func (u *credentialUses) endChange() {
	u.mu.Lock()
	changing := u.changing
	u.changing = nil
	u.mu.Unlock()
	if changing != nil {
		close(changing)
	}
}

// refuseSelectionWithLiveUses is the store's selection guard: a selection
// that moves while a credential use of the provider is alive means a path
// changed it without selectionChanging.
func (m *Manager) refuseSelectionWithLiveUses(providerName, from, to string) error {
	uses, err := m.uses(providerName)
	if err != nil {
		return err
	}
	uses.mu.Lock()
	alive := len(uses.running)
	uses.mu.Unlock()
	if alive > 0 {
		return fmt.Errorf(
			"the %s selection moved from %q to %q with %d credential use(s) still running: the change did not end them first",
			providerName, from, to, alive,
		)
	}
	return nil
}

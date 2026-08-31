package provideraccountapp

import (
	"sync"

	"agent-overflow/internal/provider"
)

// Selection is one provider's active managed-account snapshot.
type Selection struct {
	Generation uint64
	AccountID  string
	Account    provider.AccountInfo
}

// SelectionLease holds the account manager's selection read lock while a
// provider write is ordered against account activation. Release is idempotent.
type SelectionLease struct {
	Selection Selection
	release   func()
	once      sync.Once
}

// NewSelectionLease constructs a lease around a lock release callback.
// The account manager is the production owner; the exported constructor keeps
// root composition and focused contract tests free of package internals.
func NewSelectionLease(selection Selection, release func()) *SelectionLease {
	return &SelectionLease{Selection: selection, release: release}
}

// Release relinquishes the selection read lock exactly once.
func (l *SelectionLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}

// SessionGateway projects an activated provider-wide selection onto the live
// session runtime without giving account code access to App or its session map.
type SessionGateway interface {
	ApplySelection(providerName string, selection Selection)
}

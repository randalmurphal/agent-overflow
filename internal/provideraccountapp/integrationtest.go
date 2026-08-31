package provideraccountapp

import (
	"crypto/sha256"

	"agent-overflow/internal/provideraccounts"
)

// CredentialStoreForTest exposes the owned byte store only to the existing
// spawn-isolated root integration fixtures. Production code must use Manager
// operations so credential writes remain inside the transaction boundary.
func (m *Manager) CredentialStoreForTest() *provideraccounts.Credentials {
	return m.credentials
}

// MetadataStoreForTest exposes metadata setup to legacy integration fixtures.
func (m *Manager) MetadataStoreForTest() *provideraccounts.Store { return m.store }

// BlessCredentialForTest seeds the exact fingerprint production activation
// would record after a fixture installs its canonical credential.
func (m *Manager) BlessCredentialForTest(providerName string, credential []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fingerprints[providerName] = sha256.Sum256(credential)
}

// CredentialFingerprintForTest reads the process-local digest for assertions.
func (m *Manager) CredentialFingerprintForTest(providerName string) ([32]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fingerprint, ok := m.fingerprints[providerName]
	return fingerprint, ok
}

// SetAuditPathForTest redirects durable audit assertions to a fixture path.
func (m *Manager) SetAuditPathForTest(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditPath = path
}

// HoldSelectionWriteLockForTest holds the activation side for a focused
// non-blocking metadata-read assertion.
func (m *Manager) HoldSelectionWriteLockForTest() func() {
	m.mu.Lock()
	return m.mu.Unlock
}

package provideraccountapp

import (
	"crypto/sha256"
	"slices"
	"strings"
	"time"
)

// canonicalRecord is what the manager knows about one provider's canonical
// credential. recorded is the digest of the credential identified or
// activated as the selected account's login: the one proof that canonical
// bytes are that account's without asking the provider. unresolved is a
// canonical login reconciliation identified and could not attribute to one
// saved account, kept so later reconciliations of the same bytes do not
// start the identity probe again.
//
// Every read and write of either goes through the methods below, under mu.
type canonicalRecord struct {
	recorded    [32]byte
	hasRecorded bool
	unresolved  *unresolvedLogin
}

// unresolvedLogin is a canonical login that names no saved account for
// certain. It holds while the canonical credential is still those bytes and
// the saved accounts' identities are still the ones it was judged against: a
// workspace learned later, an account added or removed, can resolve it.
type unresolvedLogin struct {
	credential [32]byte
	identities [32]byte
	err        error
}

// recordCanonicalLocked records credential as the selected account's login.
// It deliberately takes the bytes the caller activated or identified rather
// than rereading the canonical store: an outside login can replace that file
// without taking mu, and recording the later value would suppress its
// reconciliation. The caller holds mu.
func (m *Manager) recordCanonicalLocked(providerName string, credential []byte) {
	m.canonical[providerName] = canonicalRecord{recorded: sha256.Sum256(credential), hasRecorded: true}
}

// canonicalRecordedLocked reports that credential is the one recorded for
// the selected account. The caller holds mu.
func (m *Manager) canonicalRecordedLocked(providerName string, credential []byte) bool {
	record := m.canonical[providerName]
	return record.hasRecorded && record.recorded == sha256.Sum256(credential)
}

// forgetCanonicalLocked drops everything known about the canonical
// credential, so the next reconciliation identifies it. The caller holds mu.
func (m *Manager) forgetCanonicalLocked(providerName string) {
	delete(m.canonical, providerName)
}

// markUnresolvedLocked records that credential's login names no saved
// account for certain, and why, as the sentence a person reads. It forgets
// any recorded credential: the canonical store no longer holds it. The
// caller holds mu.
func (m *Manager) markUnresolvedLocked(providerName string, credential []byte, err error) {
	m.canonical[providerName] = canonicalRecord{unresolved: &unresolvedLogin{
		credential: sha256.Sum256(credential),
		identities: m.savedIdentitiesLocked(providerName),
		err:        err,
	}}
}

// unresolvedLocked returns why credential's login could not be attributed,
// or nil when no standing verdict covers those bytes. The caller holds mu.
func (m *Manager) unresolvedLocked(providerName string, credential []byte) error {
	verdict := m.canonical[providerName].unresolved
	if verdict == nil || verdict.credential != sha256.Sum256(credential) ||
		verdict.identities != m.savedIdentitiesLocked(providerName) {
		return nil
	}
	return verdict.err
}

// savedIdentitiesLocked digests the identity of every saved account of one
// provider, the input an attribution is decided from. The caller holds mu.
func (m *Manager) savedIdentitiesLocked(providerName string) [32]byte {
	accounts := m.store.List(providerName, time.Now())
	lines := make([]string, 0, len(accounts))
	for _, account := range accounts {
		lines = append(lines, account.ID+"\x00"+strings.ToLower(strings.TrimSpace(account.Email))+"\x00"+strings.TrimSpace(account.OrgID))
	}
	slices.Sort(lines)
	return sha256.Sum256([]byte(strings.Join(lines, "\n")))
}

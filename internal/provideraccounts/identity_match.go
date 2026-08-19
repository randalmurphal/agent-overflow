package provideraccounts

import (
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// Identity is one observed provider login for matching purposes: who the
// provider says is signed in, on which organization. It is the ONLY
// vocabulary account resolution speaks — every "is this the saved
// account?" question routes through Confirms/Contradicts/FindByIdentity
// so blank-field semantics cannot fork per call site.
//
// Blank fields mean UNKNOWN, never "none": Claude reports no identity at
// all right after a switch (AO retires oauthAccount and the CLI re-derives
// it asynchronously), the org uuid is only available at adoption points,
// and API-key auth has no org. A blank therefore never contradicts
// anything; only two KNOWN values can disagree.
//
// Matching runs on Email and OrgID ONLY. OrgName is carried for display:
// it comes from two independent sources (the probe wire's display string
// vs the on-disk oauthAccount record) and changes on an organization
// rename, so treating a name disagreement as evidence of a different
// login would refuse valid credentials — on the usage-refresh path that
// refusal drops a just-spent single-use token rotation (the 2026-08-18
// incident shape).
type Identity struct {
	Email   string
	OrgID   string
	OrgName string
}

// IdentityFromInfo projects a probe answer onto the matching vocabulary.
func IdentityFromInfo(info provider.AccountInfo) Identity {
	return Identity{
		Email:   strings.TrimSpace(info.Email),
		OrgID:   strings.TrimSpace(info.OrgID),
		OrgName: strings.TrimSpace(info.OrgName),
	}
}

// AccountIdentity projects a saved account onto the matching vocabulary.
func AccountIdentity(account Account) Identity {
	return Identity{
		Email:   strings.TrimSpace(account.Email),
		OrgID:   strings.TrimSpace(account.OrgID),
		OrgName: strings.TrimSpace(account.OrgName),
	}
}

// normalizeIdentity re-trims a hand-constructed Identity so FindByIdentity
// gives the same answer whether the value came through a projection
// constructor or a struct literal.
func normalizeIdentity(id Identity) Identity {
	id.Email = strings.TrimSpace(id.Email)
	id.OrgID = strings.TrimSpace(id.OrgID)
	id.OrgName = strings.TrimSpace(id.OrgName)
	return id
}

// Contradicts reports whether two identities describe provably DIFFERENT
// logins. This is the guard question ("did the account change under me?"):
// a blank on either side of an axis proves nothing and never contradicts,
// and OrgName is not an axis at all — a name disagreement is a rename or
// a source-formatting difference, never proof of a different login.
func (a Identity) Contradicts(b Identity) bool {
	if a.Email != "" && b.Email != "" && !strings.EqualFold(a.Email, b.Email) {
		return true
	}
	return a.OrgID != "" && b.OrgID != "" && a.OrgID != b.OrgID
}

// Confirms reports whether two identities are positively the SAME login:
// emails are known and equal, and nothing contradicts. This is the
// keep-my-current-claim question (stronger than !Contradicts, which two
// all-blank identities satisfy), used when a caller holds an expected
// assignment and asks whether the observation re-affirms it.
func (a Identity) Confirms(b Identity) bool {
	return a.Email != "" && b.Email != "" &&
		strings.EqualFold(a.Email, b.Email) && !a.Contradicts(b)
}

// ErrAmbiguousIdentity reports that an observed identity matches more
// than one saved account and nothing observed can pick between them —
// several organizations share the email and the observation carries no
// org id. The caller decides the fallback: reconcile keeps the active
// assignment when the observation confirms it, attribution keeps its
// expected assignment, and a login surfaces the error before any slot
// write (a blank-org duplicate would be refused by the write layer, so
// there is no account to create from an ambiguous observation).
var ErrAmbiguousIdentity = errors.New("provideraccounts: observed identity matches multiple saved accounts")

// FindByIdentity resolves an observed identity to the saved account it
// describes. (Account{}, false, nil) means "no saved account — this is a
// new login". The lattice, in order:
//
//   - A blank observed email can never match (Codex documents email as
//     nullable; API-key auth has none; matching on nothing would
//     manufacture merges).
//   - Candidates are the provider's accounts with the same email
//     (case-insensitive). The store's write layer guarantees same-email
//     accounts all carry distinct non-blank org ids — EXCEPT the one
//     legacy shape: a single account with a blank org (saved before org
//     capture existed), which is then the email's only account.
//   - A sole blank-org candidate matches: it predates org capture and the
//     observation is its enrichment opportunity. This is deliberately
//     permissive in BOTH directions (an org-carrying observation adopts
//     the legacy account and enriches it; see the AGENTS.md known
//     limitation) because the alternative — refusing the only same-email
//     account — would strand every pre-org store behind a re-login.
//   - With an observed org id, only the id decides: equal id matches,
//     anything else is a different organization's login → new account.
//   - Without an observed org id (OrgName deliberately does not decide —
//     names are display strings from two independent sources): a single
//     candidate matches, several candidates are ErrAmbiguousIdentity.
func (s *Store) FindByIdentity(providerName string, observed Identity) (Account, bool, error) {
	observed = normalizeIdentity(observed)
	if observed.Email == "" {
		return Account{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var candidates []Account
	for _, account := range s.state.Providers[providerName].Accounts {
		if strings.EqualFold(strings.TrimSpace(account.Email), observed.Email) {
			candidates = append(candidates, account)
		}
	}
	if len(candidates) == 0 {
		return Account{}, false, nil
	}
	if len(candidates) == 1 && strings.TrimSpace(candidates[0].OrgID) == "" {
		return cloneAccount(candidates[0]), true, nil
	}
	if observed.OrgID != "" {
		for _, candidate := range candidates {
			if strings.TrimSpace(candidate.OrgID) == observed.OrgID {
				return cloneAccount(candidate), true, nil
			}
		}
		return Account{}, false, nil
	}
	if len(candidates) == 1 {
		return cloneAccount(candidates[0]), true, nil
	}
	return Account{}, false, fmt.Errorf(
		"%w: %s email %s has %d saved accounts and the observation names no organization",
		ErrAmbiguousIdentity, providerName, observed.Email, len(candidates),
	)
}

// conflictsWithSaved reports whether registering `account` under its own ID
// would collide with `existing` (a DIFFERENT saved account): same email
// without provably different organizations. Two accounts may share an
// email only when both carry non-blank, distinct org ids — a blank org on
// either side means "unknown", and an unknown must be resolved by matching
// (FindByIdentity) before a second same-email account may exist.
func conflictsWithSaved(account, existing Account) bool {
	email := strings.TrimSpace(account.Email)
	if email == "" || !strings.EqualFold(strings.TrimSpace(existing.Email), email) {
		return false
	}
	newOrg := strings.TrimSpace(account.OrgID)
	savedOrg := strings.TrimSpace(existing.OrgID)
	return newOrg == "" || savedOrg == "" || newOrg == savedOrg
}

// mergeOrgFields folds an incoming account write over the saved row's org
// fields, enforcing that an account's organization can never CHANGE:
//
//   - a blank incoming value preserves the saved one (blank = unknown,
//     and unknown must never erase knowledge);
//   - a first non-blank OrgID enriches a legacy blank;
//   - a DIFFERENT non-blank OrgID is refused — that observation describes
//     another organization's login and belongs on another account. The
//     refusal lives here, at the write chokepoint, so no future call site
//     can silently rebind a saved account to a different organization.
//
// OrgName is display state and follows the same blank-preserves rule but
// may change freely when known (organizations rename).
func mergeOrgFields(account, existing Account) (Account, error) {
	incoming := strings.TrimSpace(account.OrgID)
	saved := strings.TrimSpace(existing.OrgID)
	if incoming != "" && saved != "" && incoming != saved {
		return Account{}, fmt.Errorf(
			"provideraccounts: %s account %s belongs to organization %s; refusing to rebind it to %s",
			account.Provider, account.ID, saved, incoming,
		)
	}
	if incoming == "" {
		account.OrgID = existing.OrgID
	}
	if strings.TrimSpace(account.OrgName) == "" {
		account.OrgName = existing.OrgName
	}
	return account, nil
}

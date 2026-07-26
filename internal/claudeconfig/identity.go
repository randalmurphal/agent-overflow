package claudeconfig

// oauthAccountKey is Claude Code's own name for the cached identity of
// the logged-in account: account uuid, email, organization, billing
// type, and the profile fields derived from them.
const oauthAccountKey = "oauthAccount"

// ClearOAuthAccount removes the cached account identity from
// ~/.claude.json and reports whether anything was removed.
//
// This is the second half of a Claude account switch. Claude Code
// splits one login across two files: the credential store
// (~/.claude/.credentials.json) holds the tokens, and this file holds
// the identity the CLI reports and bills against. Replacing only the
// credential leaves the CLI describing — and caching entitlements for
// — the previous account.
//
// Rather than write an identity we would have to keep correct, we
// delete it the way Claude Code's own `/logout` does
// (`performLogout`: `updated.oauthAccount = undefined`). The next CLI
// start finds the key absent, fetches the profile using whichever
// token is now installed, and writes the identity back itself
// (`populateOAuthAccountInfoIfNeeded` → `storeOAuthAccountInfo`). The
// provider stays the only writer of its own identity, so AO can never
// pair one account's email with another's tokens.
//
// A missing key or missing file is not an error: both mean the
// identity is already absent, which is exactly the post-condition.
func (s *Store) ClearOAuthAccount() (bool, error) {
	return s.modifyReporting(func(root *orderedJSON) (bool, error) {
		if !root.has(oauthAccountKey) {
			return false, nil
		}
		root.delete(oauthAccountKey)
		return true, nil
	})
}

// StripOAuthAccount removes the cached account identity from a
// serialized ~/.claude.json without touching any file. It backs
// copying the config into a short-lived provider home: the copy must
// carry the user's onboarding and preference state but must NOT carry
// the canonical home's identity, or the CLI running there would
// consider its identity already resolved and never derive the one
// belonging to the credential it was actually seeded with.
//
// Input that is not a JSON object is returned unchanged — the caller
// is copying a file it does not own, and a shape we don't recognize is
// not ours to rewrite.
func StripOAuthAccount(data []byte) ([]byte, error) {
	root, err := parseOrderedJSON(data)
	if err != nil {
		return data, nil
	}
	if !root.has(oauthAccountKey) {
		return data, nil
	}
	root.delete(oauthAccountKey)
	return root.marshalIndented()
}

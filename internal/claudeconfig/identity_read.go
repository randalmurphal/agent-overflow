package claudeconfig

import (
	"encoding/json"
	"strings"
)

// OAuthAccount is the read-only slice of ~/.claude.json's oauthAccount
// record that account adoption consumes. The CLI writes the record from a
// profile fetch at login; this is the only on-disk source of the
// organization UUID — the probe wire carries just the display name.
type OAuthAccount struct {
	AccountUUID      string `json:"accountUuid"`
	EmailAddress     string `json:"emailAddress"`
	OrganizationUUID string `json:"organizationUuid"`
	OrganizationName string `json:"organizationName"`
}

// ReadOAuthAccount returns the cached identity record, or ok=false when
// the file or the record is absent or unreadable.
//
// This is a deliberate, narrow amendment to the "AO never reads
// oauthAccount" rule (identity.go): adoption may read the record ONCE, at
// the moment an account is being created or matched, to learn which
// organization the login belongs to. It must never be treated as live
// state — AO clears the record on every switch and the CLI rewrites it
// asynchronously, so absence carries no information — and the caller must
// discard the answer unless EmailMatches the identity the probe reported,
// or a racing external login could stamp one account's organization onto
// another's tokens. AO still never WRITES the record.
//
// Absence and unreadability are the same answer because every consumer
// treats "unknown organization" as compatible-with-any; failing loudly
// here would turn a mid-write race with the CLI (which rewrites this file
// on a sub-second cadence) into a failed account adoption.
func (s *Store) ReadOAuthAccount() (OAuthAccount, bool) {
	data, _, err := readFileWithStat(s.path)
	if err != nil || data == nil {
		return OAuthAccount{}, false
	}
	var root struct {
		OAuthAccount *OAuthAccount `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &root); err != nil || root.OAuthAccount == nil {
		return OAuthAccount{}, false
	}
	return *root.OAuthAccount, true
}

// EmailMatches reports whether the record describes the same login as the
// probe-reported email. A blank on either side is a mismatch: the caller
// is deciding whether to trust this record's organization fields, and an
// unpaired record must not be trusted.
func (o OAuthAccount) EmailMatches(email string) bool {
	email = strings.TrimSpace(email)
	recorded := strings.TrimSpace(o.EmailAddress)
	return email != "" && recorded != "" && strings.EqualFold(recorded, email)
}

package provideraccounts

import (
	"fmt"

	"agent-overflow/internal/claudeconfig"
)

// retireProviderIdentity drops any provider-side record of WHICH account
// is logged in, leaving only the credential itself to answer that.
//
// Claude Code splits one login across two files. `.credentials.json`
// holds the tokens; `~/.claude.json`'s `oauthAccount` holds the identity
// the CLI reports, bills against, and caches entitlements for. The
// identity is written from a profile fetch at login, never derived from
// the token on read, so swapping the credential alone leaves the CLI
// describing the previous account indefinitely — and leaves anything
// that asks the CLI who it is (including Agent Overflow's own account
// probe) answering with the wrong account.
//
// Deleting the record is what Claude Code's own `/logout` does. On its
// next start the CLI finds it absent, fetches the profile with whichever
// token is now installed, and writes the identity back itself. That
// keeps the provider the sole author of its own identity: Agent Overflow
// never has an identity of its own to keep in sync, and cannot pair one
// account's email with another account's tokens.
//
// Codex needs no equivalent — its `auth.json` carries the account claims
// inside the credential, so replacing the file replaces the identity.
func (c *Credentials) retireProviderIdentity(providerName string) error {
	paths, err := c.Paths(providerName)
	if err != nil {
		return err
	}
	if paths.GlobalConfig == "" {
		return nil
	}
	if _, err := claudeconfig.New(paths.GlobalConfig).ClearOAuthAccount(); err != nil {
		return fmt.Errorf(
			"provideraccounts: retire %s account identity: %w",
			providerName,
			err,
		)
	}
	return nil
}

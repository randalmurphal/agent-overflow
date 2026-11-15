package harnessrpc

import (
	"errors"
	"fmt"

	"agent-overflow/internal/providerupdate/updatefake"
)

// HarnessProviderUpdateFeedSpec is the release the isolated provider
// release hosts advertise.
type HarnessProviderUpdateFeedSpec struct {
	// Latest is the version both providers' release hosts advertise and
	// the mock provider's update installs: one of updatefake.Stable
	// ("99.0.0", "99.0.1", "99.1.0"). Empty restores the default, the mock
	// provider's own 99.0.0, which offers no update.
	Latest string `json:"latest"`
}

// HarnessSetProviderUpdateFeed sets the version the isolated release hosts
// advertise for Claude and Codex, and makes it what the mock provider's
// update installs. It checks nothing: a caller follows it with
// CheckProviderUpdates. Reset restores the default and the mock's baseline
// install.
func (h *Harness) HarnessSetProviderUpdateFeed(spec HarnessProviderUpdateFeedSpec) error {
	if h.config.Host == nil {
		return errors.New("harness host unavailable")
	}
	feed := h.config.Host.ProviderUpdateFeed()
	if feed == nil {
		return errors.New("this boot has no isolated provider release hosts")
	}
	install, err := h.mockInstall()
	if err != nil {
		return err
	}
	if spec.Latest == "" {
		feed.Reset()
		return install.SetTarget("")
	}
	if err := feed.SetLatest(spec.Latest); err != nil {
		return err
	}
	if err := install.SetTarget(spec.Latest); err != nil {
		feed.Reset()
		return fmt.Errorf("set the mock provider's update target: %w", err)
	}
	return nil
}

// mockInstall is the install the mock provider simulates for this boot. It
// lives in the harness home, so a boot that keeps the real home has none.
func (h *Harness) mockInstall() (updatefake.MockInstall, error) {
	if h.config.HomeDir == "" {
		return updatefake.MockInstall{}, errors.New("provider update seeding needs the harness home, which AO_HARNESS_KEEP_HOME replaces with the real one")
	}
	if h.config.MockProvider == "" {
		return updatefake.MockInstall{}, errors.New("this boot has no mock provider")
	}
	return updatefake.MockInstall{Home: h.config.HomeDir, Executable: h.config.MockProvider}, nil
}

// resetProviderUpdates restores the default feed and the mock provider's
// baseline install. Restoring the install moves the mock's mtime, so the
// app reads the installed version again; when anything was restored the
// providers are checked again, so the state every later spec hydrates from
// offers no update.
func (h *Harness) resetProviderUpdates() error {
	if h.config.Host == nil {
		return nil
	}
	feed := h.config.Host.ProviderUpdateFeed()
	if feed == nil {
		return nil
	}
	changed := feed.Reset()
	if install, err := h.mockInstall(); err == nil {
		restored, err := install.Reset()
		if err != nil {
			return fmt.Errorf("restore the mock provider's install: %w", err)
		}
		changed = changed || restored
	}
	if changed {
		h.config.Host.RecheckProviderUpdates()
	}
	return nil
}

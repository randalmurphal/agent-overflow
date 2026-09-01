// Package providerstatus carries the wire shape and pure mapping
// helpers behind the `provider:status` event channel.
//
// Provider-level health (install / version / auth) is pushed to the
// frontend so ProviderStatusBanner can render actionable guidance.
// This is a separate channel from the triage router's per-turn
// `provider:item_event` stream — those describe timeline content;
// these describe the binary itself.
package providerstatus

import (
	"strings"

	"agent-overflow/internal/provider"
)

// Event is the payload for the `provider:status` wire event. The JSON
// tags pin the wire shape; relocating the Go type doesn't change what
// reaches the frontend.
type Event struct {
	// Provider is "claude" or "codex".
	Provider string `json:"provider"`

	// Status mirrors provider.ProviderStatus.Status values:
	//   "ready", "not_found", "version_too_old", "unauthenticated", "error".
	Status string `json:"status"`

	// Message is the human-friendly explanation. Always populated for
	// non-ready statuses; callers should treat it as the text the UI
	// will show if it has no status-specific branch.
	Message string `json:"message,omitempty"`

	// Version is the raw version string (e.g. "codex 0.36.0"). Empty
	// when unknown or irrelevant to the status.
	Version string `json:"version,omitempty"`

	// Actionable means the UI should render a primary action button
	// (e.g. "Install Claude CLI", "Authenticate with Claude"). When
	// false the banner stays informational.
	Actionable bool `json:"actionable"`

	// ActionURL is a deep-link / external URL the UI can open from
	// the primary action button. Empty when no URL makes sense (for
	// version_too_old the guidance is "upgrade + restart the app",
	// which doesn't map to a single URL).
	ActionURL string `json:"actionUrl,omitempty"`

	// Kind is the closed vocabulary the frontend branches on BEFORE
	// falling back to Status; an emission that sets it may leave Status
	// empty. An unknown kind is dropped by the frontend router, so a new
	// value here needs its frontend branch in the same change.
	Kind string `json:"kind,omitempty"`

	// ThreadID scopes the event to one pane. Empty means the event is
	// provider-global and fans out to every pane on that provider —
	// which is what every binary-detect emission is.
	ThreadID string `json:"threadId,omitempty"`

	// SessionVersion / InstalledVersion carry the two sides of a
	// KindBinaryStale comparison: the version the live provider process
	// reported about itself, and the version the binary on disk reports
	// now. Empty on every other kind.
	SessionVersion   string `json:"sessionVersion,omitempty"`
	InstalledVersion string `json:"installedVersion,omitempty"`
}

// KindBinaryStale marks a thread whose live provider process is running a
// build older (or simply other) than the binary now on disk — the user
// upgraded the CLI underneath a running session. Thread-scoped by
// construction: the process, not the provider, is what went stale.
const KindBinaryStale = "binary_stale"

// VersionToken extracts the leading dotted-decimal version token from a
// version string. `claude --version` prints "2.1.257 (Claude Code)",
// `codex --version` prints "codex-cli 0.149.0", a Claude session reports a
// bare "2.1.257" on `system/init`, and a Codex session reports a bare
// "0.149.0" off its `initialize` user agent — one rule reads all four.
//
// Returns "" when the string carries no numeric token, which every caller
// must read as "unknown", never as a version that differs from another.
func VersionToken(raw string) string {
	for i := 0; i < len(raw); i++ {
		if !isASCIIDigit(raw[i]) {
			continue
		}
		end := i
		lastDigit := i
		for end < len(raw) && (isASCIIDigit(raw[end]) || raw[end] == '.') {
			if isASCIIDigit(raw[end]) {
				lastDigit = end
			}
			end++
		}
		// Trim a trailing '.' so "1.2." tokenizes as "1.2".
		return raw[i : lastDigit+1]
	}
	return ""
}

// BinaryStale reports whether a live session's version and the installed
// binary's version are both known AND different.
//
// Absence is not a denial: an unknown version on either side answers false.
// A session that never reported its build, or a `--version` run that
// produced nothing parseable, is not evidence that anything changed — and
// a banner telling a user to restart a healthy session is worse than a
// missing one.
//
// Comparison is per numeric segment with trailing zero segments dropped, so
// "0.149" and "0.149.0" are the same build. The two sides are read from
// different surfaces (a `--version` line vs. a handshake field) and one of
// them already normalizes two-segment versions to three, so a textual
// compare would report a fabricated upgrade.
func BinaryStale(sessionVersion, installedVersion string) bool {
	session := VersionToken(sessionVersion)
	installed := VersionToken(installedVersion)
	if session == "" || installed == "" {
		return false
	}
	return !sameVersionToken(session, installed)
}

func sameVersionToken(a, b string) bool {
	left, right := trimZeroSegments(strings.Split(a, ".")), trimZeroSegments(strings.Split(b, "."))
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if strings.TrimLeft(left[i], "0") != strings.TrimLeft(right[i], "0") {
			return false
		}
	}
	return true
}

func trimZeroSegments(segments []string) []string {
	for len(segments) > 0 && strings.TrimLeft(segments[len(segments)-1], "0") == "" {
		segments = segments[:len(segments)-1]
	}
	return segments
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// ActionURL returns the canonical docs / login URL for a given
// provider + status pair. Keeping the list here means the frontend
// can't invent its own URLs — every banner's primary action comes
// from this table.
func ActionURL(providerName, status string) string {
	switch providerName {
	case string(provider.Claude):
		switch status {
		case "not_found":
			return "https://docs.claude.com/en/docs/claude-code/setup"
		case "unauthenticated":
			// `claude login` is a shell instruction, not a URL; the
			// installation docs cover sign-in too, so point there.
			return "https://docs.claude.com/en/docs/claude-code/setup"
		}
	case string(provider.Codex):
		switch status {
		case "not_found":
			return "https://github.com/openai/codex#installation"
		}
	}
	return ""
}

// EventFromDetect converts the pull-only ProviderStatus returned by
// DetectProvider into the push-shape used on the wire. The "ready"
// case short-circuits to an all-default event because the UI treats
// any status=="ready" payload as "clear the banner, no action needed".
func EventFromDetect(ps provider.ProviderStatus) Event {
	if ps.Status == "ready" {
		return Event{
			Provider: ps.Provider,
			Status:   "ready",
			Version:  ps.Version,
		}
	}
	return Event{
		Provider:   ps.Provider,
		Status:     ps.Status,
		Message:    ps.Message,
		Version:    ps.Version,
		Actionable: true,
		ActionURL:  ActionURL(ps.Provider, ps.Status),
	}
}

// claudeFirstPartyAPIProvider is the one `apiProvider` value for which
// the Claude CLI populates the rest of the account object. Every other
// member of the enum (gateway, bedrock, foundry, anthropicAws,
// anthropicGoogleCloud, mantle, vertex) short-circuits the builder.
const claudeFirstPartyAPIProvider = "firstParty"

// ClaudeUnauthenticated reports whether a Claude `ProbeAccount` result
// carries no evidence of a usable account — the logged-out signal that
// routes users to `claude login`.
//
// The rule is "any identity evidence at all means authenticated",
// because the CLI's account-metadata builder is far stingier than the
// wire shape suggests. Verified against claude 2.1.219: the builder
// returns early unless the resolved API provider is "firstParty", so
// for the other seven enum members (gateway, bedrock, foundry,
// anthropicAws, anthropicGoogleCloud, mantle, vertex) a fully working
// account surfaces `apiProvider` and nothing else. Within firstParty, a
// profile-sourced token populates no `tokenSource` — `email` is the only
// field that comes back. Requiring a token source would therefore report
// every external-credential backend, and every profile login, as logged
// out.
//
// Evidence is `email`, `displayName`, or a real `tokenSource`. Three
// fields that look like evidence are not:
//
// A bare `apiProvider:"firstParty"` is NOT evidence: that is the one
// value the builder emits before deciding it has nothing to add, so it
// is exactly what a signed-out first-party CLI reports.
//
// `tokenSource:"none"` is NOT evidence either — it is the CLI's explicit
// no-token marker, not a token source. Spike-verified on 2.1.219
// (2026-08-03): a startup token refresh that fails with invalid_grant
// blanks the stored tokens in place, and the very same probe then
// answers initialize with `{tokenSource:"none", apiProvider:"firstParty"}`
// and exit 0. Reading that "none" as identity evidence reported a
// just-destroyed login as authenticated, which let the destructive
// refresh failure surface only as downstream 401s.
//
// `subscriptionType` is NOT evidence: it is a storage echo, not identity.
// Spike-verified on 2.1.232 (2026-08-16): the husk the CLI writes on
// invalid_grant blanks the tokens but RETAINS the other credential
// fields, and the zero-turn probe then answers initialize with
// `{subscriptionType:"Claude Max", apiProvider:"firstParty"}` — a plan
// label for a login that no longer exists. Counting it defeated this
// predicate at the one moment it matters: app code read "authenticated",
// committed the husk over the account's credential slot, and destroyed
// it. A real subscription login also reports `email`, so nothing that
// was authenticated by this rule stops being so.
//
// Claude-only by contract. Codex hardcodes `apiProvider:"openai"` even
// when it knows nothing about the account, so a Codex AccountInfo would
// always read as authenticated here — `app_codex_probe.go` deliberately
// leaves its unauthenticated hook nil rather than reusing this.
// NOT a credential check. It reports what the CLI SAID about itself, and the
// CLI derives that from `~/.claude.json`'s `oauthAccount` record — which
// provideraccounts.retireProviderIdentity deletes on every account switch so
// the provider re-derives it rather than describing the outgoing account over
// the incoming account's tokens. The CLI restores it from an asynchronous
// profile fetch, so the first probe after a switch reports nothing no matter
// how healthy the login is.
//
// Spike-verified against 2.1.234: with the record absent, a healthy Claude Max
// login probes as {"subscriptionType":"Claude Max","apiProvider":"firstParty"}
// — byte-for-byte the shape a DESTROYED login echoes. The two are not
// distinguishable here, at all, ever.
//
// So this is never the whole answer. Pair it with the credential: absent or the
// sign-out husk means logged out, bytes that exist mean logged in. Both callers
// do — emitUnauthenticatedIfNoLogin gates the banner, and
// probeSelectedClaudeRateLimits reads the bytes then asks the server. Skipping
// that pairing reported healthy accounts as expired, discarded a single-use
// rotation that had already landed, and told users to re-run `claude login`
// over a working login.
func ClaudeUnauthenticated(info provider.AccountInfo) bool {
	// Trim so a whitespace-only field (a CLI that pads, a caller that
	// hand-builds an AccountInfo) can't masquerade as identity evidence.
	// The persisted-account path in app_provider_account_bindings.go already
	// trims; probe results do not.
	tokenSource := strings.TrimSpace(info.TokenSource)
	if strings.EqualFold(tokenSource, "none") {
		tokenSource = ""
	}
	if tokenSource != "" ||
		strings.TrimSpace(info.Email) != "" ||
		strings.TrimSpace(info.DisplayName) != "" {
		return false
	}
	apiProvider := strings.TrimSpace(info.APIProvider)
	return apiProvider == "" || apiProvider == claudeFirstPartyAPIProvider
}

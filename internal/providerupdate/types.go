package providerupdate

// Run phases. A provider with no update run since boot is PhaseIdle.
const (
	PhaseIdle      = "idle"
	PhaseRunning   = "running"
	PhaseSucceeded = "succeeded"
	PhaseFailed    = "failed"
)

// Status is one provider CLI's update state on this computer. It is the
// element of GetProviderUpdates and of the provider:updates event, which
// both carry every provider's Status.
type Status struct {
	// Provider is "claude" or "codex".
	Provider string `json:"provider"`
	// InstalledVersion is the version token of the binary sessions spawn,
	// "" when it could not be read.
	InstalledVersion string `json:"installedVersion"`
	// LatestVersion is the version the provider's own update command would
	// install, "" until a check has answered.
	LatestVersion string `json:"latestVersion"`
	// Available is true only when LatestVersion is newer than
	// InstalledVersion. A release channel behind the installed version is
	// not an update.
	Available bool `json:"available"`
	// CheckedAt is the unix millisecond time of the last completed check,
	// 0 before the first.
	CheckedAt int64 `json:"checkedAt"`
	// CheckError is why the last check could not answer, "" when it did.
	CheckError string `json:"checkError,omitempty"`
	// SignInRequired reports that the last check asked nothing because the
	// provider's login has expired: `claude doctor` would renew the token,
	// and a renewal past the login's refresh deadline blanks the saved
	// credential. Nothing is offered until a check after a sign-in answers.
	SignInRequired bool `json:"signInRequired,omitempty"`
	Run            Run  `json:"run"`
}

// Run is the most recent run of a provider's update command.
type Run struct {
	// Phase is one of the Phase constants.
	Phase string `json:"phase"`
	// FromVersion is the installed version when the run started.
	FromVersion string `json:"fromVersion,omitempty"`
	// ToVersion is the installed version read after the command exited.
	ToVersion string `json:"toVersion,omitempty"`
	// Error is why a failed run failed.
	Error string `json:"error,omitempty"`
	// Output is the end of the command's combined output, bounded.
	Output string `json:"output,omitempty"`
}

// Notes are the release notes between the installed version and the latest
// one, newest first.
type Notes struct {
	Provider    string `json:"provider"`
	FromVersion string `json:"fromVersion"`
	ToVersion   string `json:"toVersion"`
	Releases    []Note `json:"releases"`
	// Omitted counts older versions in the range that were left out to
	// bound the answer.
	Omitted int `json:"omitted"`
	// URL is the provider's releases page.
	URL string `json:"url"`
}

// Note is one published version's release notes.
type Note struct {
	Version string `json:"version"`
	Name    string `json:"name"`
	// PublishedAt is RFC 3339, "" when the host did not say.
	PublishedAt string `json:"publishedAt"`
	// Body is the release's Markdown as published.
	Body string `json:"body"`
	URL  string `json:"url"`
}

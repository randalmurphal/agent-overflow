// Package providerupdate tells the user when a newer Claude Code or Codex
// CLI is published, shows the release notes in between, and runs the
// updater that owns the install.
//
// A check reads the installed version from the app, then plans the update
// of that install: the updater that owns it, the version it would install
// and the command that installs it. A binary in a Homebrew Caskroom is
// upgraded by that Homebrew (`brew upgrade --cask`) to the cask's version.
// Any other Claude runs `claude update`, which installs the version of the
// release channel `claude doctor` reports. Codex behind an npm launcher
// runs the launcher's `codex update`, which installs npm's latest. Codex
// the Codex installer unpacked runs `codex update` with CODEX_RELEASE
// pinned to the latest GitHub release. Any other Codex has no updater, and
// its check fails saying so. An update is offered only when the planned
// version is newer than the installed one, because Claude's updater
// installs the channel's version even when it is older. A failed check
// clears the latest version and reports why in CheckError; one provider's
// failure never blocks the other's check.
//
// The Claude CLI is started only through claude.NewCommandLine, as every
// Claude launch is, so `claude doctor` and `claude update` carry the
// transcript retention. No doctor runs while the Claude login is past its
// refresh deadline: the check clears the latest version and reports
// SignInRequired instead of an error. A doctor is stopped once it has
// printed its channel.
//
// A check describes one install: it reads the binary and environment once,
// uses them for every command it runs and for the update it plans, and
// fails when the binary or the installed version differs at its end.
//
// A run checks again before it runs anything and runs the update that check
// planned only if it still offers a newer version. The outcome is judged by
// the installed version read afterwards, and a run fails unconfirmed when
// the configured binary changed while the updater ran. An update command
// that could not be stopped refuses every later run of its provider for the
// life of the process.
//
// Release notes come from GitHub: one matching-refs listing narrowed by
// the two versions' shared leading components, then one release per stable
// tag in the range, newest first and capped. The last answer per provider
// is cached for the version pair it describes.
//
// The service owns its state in memory, an hourly check loop, checks,
// notes lookups and update runs. Every state change emits every provider's
// Status through Deps.Emit, in mutation order. Stop cancels and joins all
// of it and refuses what comes after.
package providerupdate

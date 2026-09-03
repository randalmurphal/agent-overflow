// Package serviceinstall installs `agent-overflow serve` as a per-user
// background service: a systemd user unit on Linux, a launchd LaunchAgent on
// macOS.
//
// It generates the unit file, hands the manager its own commands, and reports
// what the manager says back. It knows nothing about the backend it supervises
// beyond the path to the binary and the arguments to start it with.
//
// Two design choices carry the package.
//
// The host is a STRING (Config.GOOS), not a build tag. Both unit-file formats
// therefore generate and golden-test on any machine, which is the only way the
// launchd plist gets reviewed by anyone who is not sitting at a Mac. Only the
// commands are platform-bound, and those never run in a test.
//
// Every external command goes through Runner, which is required — New refuses
// a nil one. There is exactly one real implementation (ExecRunner), tests pass
// a fake, and a test that forgot to would fail at construction rather than by
// enabling a service on the developer's machine.
package serviceinstall

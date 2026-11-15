package main

import (
	"fmt"
	"os"
	"runtime"

	"agent-overflow/internal/providerupdate/updatefake"
)

// The CLI update one-shots: `claude doctor`, `claude update`, and Codex's
// `codex update`. Both providers resolve to this binary in the harness, so
// `update` serves both with Claude's report shape. The simulated install,
// what is installed and what an update installs, is
// updatefake.MockInstall under the home directory.

func isDoctorInvocation(args []string) bool {
	args = withoutSettingsBlock(args)
	return len(args) == 1 && args[0] == "doctor"
}

func isUpdateInvocation(args []string) bool {
	args = withoutSettingsBlock(args)
	return len(args) == 1 && args[0] == "update"
}

// withoutSettingsBlock drops the leading `--settings <json>` a Claude
// command line puts ahead of its subcommand (claude.NewCommandLine).
func withoutSettingsBlock(args []string) []string {
	if len(args) >= 2 && args[0] == "--settings" {
		return args[2:]
	}
	return args
}

// mockInstall is the install this process simulates.
func mockInstall() (updatefake.MockInstall, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return updatefake.MockInstall{}, fmt.Errorf("resolve home: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return updatefake.MockInstall{}, fmt.Errorf("locate executable: %w", err)
	}
	return updatefake.MockInstall{Home: home, Executable: exe}, nil
}

// installedVersion is the version an update recorded, else
// mockVersionNumber. It never fails: a missing home means no update ran,
// and an unreadable or malformed record is reported on stderr and ignored.
func installedVersion() string {
	install, err := mockInstall()
	if err != nil {
		return mockVersionNumber
	}
	version, err := install.Installed()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ao-mockprovider: read installed version: %v; reporting %s\n", err, version)
	}
	return version
}

// runDoctor prints the non-TTY `claude doctor` report. The app reads only
// the `Auto-update channel:` line.
func runDoctor() {
	fmt.Printf("Claude Code doctor\n\nRunning: native (%s)\nPlatform: %s\nConfig install method: native\nAuto-update channel: latest\n",
		installedVersion(), doctorPlatform())
}

// doctorPlatform spells the platform the way Node's process.platform and
// process.arch do.
func doctorPlatform() string {
	goos := runtime.GOOS
	if goos == "windows" {
		goos = "win32"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	}
	return goos + "-" + arch
}

// runUpdate prints the non-TTY `claude update` report and installs the
// advertised target. With none advertised, or the target installed, it
// changes nothing. It exits non-zero when the install cannot be read or
// recorded.
func runUpdate() {
	install, err := mockInstall()
	if err != nil {
		updateFailed(err)
	}
	current := installedVersion()
	fmt.Printf("Current version: %s\nChecking for updates to latest version...\n", current)
	target, err := install.Target()
	if err != nil {
		updateFailed(err)
	}
	if target == "" || target == current {
		fmt.Printf("Claude Code is up to date (%s)\n", current)
		return
	}
	fmt.Printf("Updating to %s...\n", target)
	if err := install.Install(target); err != nil {
		updateFailed(err)
	}
	fmt.Printf("Successfully updated from %s to version %s\n", current, target)
}

func updateFailed(err error) {
	fmt.Fprintf(os.Stderr, "ao-mockprovider: update failed: %v\n", err)
	os.Exit(1)
}

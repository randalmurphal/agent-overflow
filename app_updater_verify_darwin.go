//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type macUpdateCommand func(name string, args ...string) ([]byte, error)

// verifyStagedDesktopUpdate is the final macOS trust boundary before Wails'
// helper swaps directories. The release checksum proves the downloaded bytes
// match GitHub; these checks independently prove the extracted app is intact,
// notarized, accepted by Gatekeeper, and signed under the same designated
// requirement as the app currently running.
func verifyStagedDesktopUpdate(staged string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running executable: %w", err)
	}
	current, ok := enclosingAppBundle(executable)
	if !ok {
		return fmt.Errorf("running executable is not inside a macOS app bundle")
	}
	return verifyMacUpdateWith(current, staged, func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	})
}

func verifyMacUpdateWith(current, staged string, run macUpdateCommand) error {
	if filepath.Ext(current) != ".app" || filepath.Ext(staged) != ".app" {
		return fmt.Errorf("current and staged updates must both be .app bundles")
	}
	if output, err := run("/usr/bin/codesign", "--verify", "--deep", "--strict", staged); err != nil {
		return commandFailure("verify code signature", output, err)
	}
	currentRequirement, err := macDesignatedRequirement(current, run)
	if err != nil {
		return fmt.Errorf("read current app signing requirement: %w", err)
	}
	stagedRequirement, err := macDesignatedRequirement(staged, run)
	if err != nil {
		return fmt.Errorf("read staged app signing requirement: %w", err)
	}
	if currentRequirement != stagedRequirement {
		return fmt.Errorf("staged app signing requirement does not match the running app")
	}
	if output, err := run("/usr/bin/xcrun", "stapler", "validate", staged); err != nil {
		return commandFailure("validate stapled notarization ticket", output, err)
	}
	if output, err := run("/usr/sbin/spctl", "--assess", "--type", "execute", staged); err != nil {
		return commandFailure("assess with Gatekeeper", output, err)
	}
	return nil
}

func macDesignatedRequirement(app string, run macUpdateCommand) (string, error) {
	output, err := run("/usr/bin/codesign", "--display", "--requirements", "-", app)
	if err != nil {
		return "", commandFailure("read designated requirement", output, err)
	}
	const marker = "designated =>"
	line := strings.TrimSpace(string(output))
	if at := strings.Index(line, marker); at >= 0 {
		line = strings.TrimSpace(line[at+len(marker):])
	}
	if line == "" {
		return "", fmt.Errorf("codesign returned an empty designated requirement")
	}
	return line, nil
}

func commandFailure(action string, output []byte, err error) error {
	detail := string(bytes.TrimSpace(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w (%s)", action, err, detail)
}

func enclosingAppBundle(path string) (string, bool) {
	path = filepath.Clean(path)
	for {
		if filepath.Ext(path) == ".app" {
			return path, true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", false
		}
		path = parent
	}
}

//go:build !linux && !darwin

package browser

import (
	"errors"
	"os/exec"
	"time"
)

// The headless engine runs on Linux and macOS. Elsewhere Chromium gets no
// process group, stop kills the browser process alone, and the process is
// reaped as soon as it exits.

func configureChromiumProcess(*exec.Cmd) {}

func killChromium(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func awaitExit(int) error { return errors.ErrUnsupported }

func waitGroupExited(int, time.Duration) error { return nil }

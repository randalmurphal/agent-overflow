//go:build !windows

package supervise

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func crossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// replaceBundle is the macOS publish of a bundle: swap exchanges the staged
// and installed bundles in one step, leaving the previous version at the
// staged path. A filesystem that cannot swap (not APFS) takes two renames
// through previous; an interruption between them leaves the install path
// empty until a repeat finishes them.
func replaceBundle(staged, install, previous string, swap func(from, to string) error) error {
	err := swap(staged, install)
	if err == nil || !(errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL)) {
		return err
	}
	return replaceByRenames(staged, install, previous)
}

// replaceByRenames moves the installed version to previous and the staged
// one into its place. Each rename is skipped once done, so a repeat
// finishes an interrupted one.
func replaceByRenames(staged, install, previous string) error {
	if _, err := os.Lstat(install); err == nil {
		if err := os.Rename(install, previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(staged, install)
}

// StartDesktopHelper starts the helper detached, in its own session, with
// its output appended to logPath (OpenDesktopUpdateLog). env is its
// environment; nil inherits this process's.
func StartDesktopHelper(executable string, args []string, logPath string, env []string) error {
	out, err := OpenDesktopUpdateLog(logPath)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.Command(executable, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = env
	return startDetached(cmd)
}

// StartDesktopApp starts the app at installPath detached with args
// (DesktopLaunchCommand). env is its environment; nil inherits this
// process's.
func StartDesktopApp(installPath string, args []string, goos string, env []string) error {
	cmd := DesktopLaunchCommand(installPath, args, goos)
	cmd.Env = env
	return startDetached(cmd)
}

func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

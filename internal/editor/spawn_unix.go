//go:build !windows

package editor

import (
	"os"
	"os/exec"
	"syscall"
)

// applyDetachAttrs puts the editor in its own process group via
// Setpgid so it survives the parent process exiting. This mirrors
// internal/provider/process_unix.go's pattern; we duplicate it rather
// than import it because the provider package's helper signals the
// group on shutdown — open-in-editor wants the opposite, "outlive me",
// which would be the wrong contract to bolt onto a provider helper.
func applyDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	// Setsid would also detach from the controlling terminal, which
	// is what GUI editors actually want — none of them care about
	// stdin from us. We keep the default stdin (already nil because
	// SpawnOptions never sets it) and don't request a session leader,
	// which avoids the rare permission issue Setsid hits when the
	// parent already leads its own session.
}

// openDevNull returns an os.File pointing at /dev/null, opened
// read/write. Used as the editor's stdout/stderr so its log noise
// doesn't get attributed to our process.
func openDevNull() (*os.File, error) {
	return os.OpenFile(os.DevNull, os.O_RDWR, 0)
}

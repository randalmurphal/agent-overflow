//go:build windows

package editor

import (
	"os"
	"os/exec"
	"syscall"
)

// applyDetachAttrs configures the spawn so the editor survives the
// caller exiting. On Windows there is no Setpgid analogue; the
// closest equivalent is HideWindow + DETACHED_PROCESS so the editor
// doesn't borrow our console window for output. We deliberately do
// not use cmd.exe / .cmd shims here — the editor binary is invoked
// directly by absolute path so quoting bugs around paths with spaces
// can't matter.
func applyDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x00000008 // DETACHED_PROCESS
}

// openDevNull returns a NUL handle for the editor's stdout/stderr.
// Mirrors the unix /dev/null path; Go normalises os.DevNull to "NUL"
// on Windows so we get the right device either way.
func openDevNull() (*os.File, error) {
	return os.OpenFile(os.DevNull, os.O_RDWR, 0)
}

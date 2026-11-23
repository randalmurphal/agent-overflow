package main

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal ends a `go test` this process started if this
// process dies without forwarding a signal to it, as when it is killed.
func setParentDeathSignal(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

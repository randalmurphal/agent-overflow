//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// configureParentDeath sends the suite SIGTERM when the launcher dies, so a
// launcher killed outright, which tears nothing down, does not leave the
// suite running; Playwright stops its workers on SIGTERM. The signal is tied
// to the thread that starts the suite, which run locks for its whole life.
// An isolated suite gets the same from netisolate.Command.
func configureParentDeath(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Pdeathsig = syscall.SIGTERM
}

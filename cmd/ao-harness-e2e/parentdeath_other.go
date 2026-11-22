//go:build !linux

package main

import "os/exec"

// configureParentDeath does nothing: only Linux has a parent-death signal.
// Windows ends the suite through its Job Object.
func configureParentDeath(*exec.Cmd) {}

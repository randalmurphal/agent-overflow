//go:build !linux

package main

import "os/exec"

// setParentDeathSignal does nothing where the kernel has no parent-death
// signal; a signal this process receives still reaches every `go test`.
func setParentDeathSignal(*exec.Cmd) {}

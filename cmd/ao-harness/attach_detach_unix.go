//go:build !windows

package main

import "os/exec"

// applyAttachDetachAttrs has nothing to add on Unix: procutil.ConfigureGroup
// already starts the browser in its own session, so it survives this CLI
// returning and its group-kill teardown is unchanged.
func applyAttachDetachAttrs(*exec.Cmd) {}

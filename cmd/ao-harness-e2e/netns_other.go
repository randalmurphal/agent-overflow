//go:build !linux

package main

import (
	"fmt"
	"io"
	"os/exec"
)

// Network namespaces are Linux-only. Other hosts run the suite on the host
// network, as they did before isolation existed.
func isolateNetwork(*exec.Cmd) error { return nil }

func checkNetworkIsolation(io.Writer) error { return nil }

func runNetnsHelper(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "ao-harness-e2e: network isolation is Linux-only")
	return 2
}

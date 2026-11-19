//go:build !linux

package netisolate

import (
	"fmt"
	"io"
	"os/exec"
)

// Network namespaces are Linux-only. Other hosts run commands on the host
// network, with the host's temp directory and priority, as they did before
// isolation existed.
func Command(*exec.Cmd, io.Writer) (func() error, error) {
	return func() error { return nil }, nil
}

func Check(io.Writer) error { return nil }

func RunHelper(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "netisolate: network isolation is Linux-only")
	return 2
}

// Contained is false: without a namespace a process shares the host network.
func Contained() bool { return false }

// Inside is false for the same reason.
func Inside() bool { return false }

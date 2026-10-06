//go:build !linux

package netisolate

import (
	"fmt"
	"io"
	"os/exec"
)

// Network namespaces are Linux-only. Other hosts run commands on the host
// network, as they did before isolation existed.
func Command(*exec.Cmd) error { return nil }

func Check(io.Writer) error { return nil }

func RunHelper(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "netisolate: network isolation is Linux-only")
	return 2
}

// Contained is false: without a namespace a process shares the host network.
func Contained() bool { return false }

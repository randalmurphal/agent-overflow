//go:build !darwin

package orphanreaper

import "os/exec"

func applySidecarProcessAttrs(cmd *exec.Cmd) {}

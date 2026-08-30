//go:build !linux && !windows && !darwin

package containment

import (
	"os/exec"
)

type unsupportedGroup struct{}

func (unsupportedGroup) Configure(*exec.Cmd) error { return ErrUnsupported }
func (unsupportedGroup) Adopt(*exec.Cmd) error     { return ErrUnsupported }
func (unsupportedGroup) Close() error              { return nil }

func Prepare(uint64) (Group, error) { return nil, ErrUnsupported }

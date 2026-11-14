//go:build windows

package procutil

import (
	"errors"
	"os/exec"
)

// Terminal has no Windows implementation: commands that take one run in the
// Linux backend (see ConfigureGroup).
type Terminal struct{}

var errNoTerminal = errors.New("procutil: a command terminal is not supported on Windows")

func OpenTerminal() (*Terminal, error) { return nil, errNoTerminal }

func (t *Terminal) Read([]byte) (int, error)  { return 0, errNoTerminal }
func (t *Terminal) Write([]byte) (int, error) { return 0, errNoTerminal }
func (t *Terminal) Close() error              { return nil }

func (t *Terminal) command([]string, string) ([]string, error) { return nil, errNoTerminal }
func (t *Terminal) attach(*exec.Cmd)                           {}

//go:build !darwin

package darwinbundle

import "errors"

var ErrUnsupported = errors.New("darwin harness bundle is only available on macOS")

func BundleID(string, ...string) string                        { return "" }
func Create(string, string, string, ...string) (string, error) { return "", ErrUnsupported }
func Verify(string, string, string, ...string) error           { return ErrUnsupported }

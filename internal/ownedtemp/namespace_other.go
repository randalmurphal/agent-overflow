//go:build !linux

package ownedtemp

// Only Linux keeps a directory for a mount namespace's processes.
func mountNamespaceInUse(string) (bool, error) { return false, nil }

//go:build windows

package main

import "testing"

func TestValidateWindowsStoragePathRejectsEmpty(t *testing.T) {
	if err := validateWindowsStoragePath(" "); err == nil {
		t.Fatal("validateWindowsStoragePath accepted an empty path")
	}
}

//go:build !unix

package main

import "os"

// fileIdentity has no cheap answer off unix: Windows would need a
// GetFileInformationByHandle round trip on an open handle, which is a
// syscall and a handle per file per check for a heuristic the size
// comparison already covers in the common case. Empty means "no
// identity", and scanNewLines falls back to the size heuristic alone.
func fileIdentity(os.FileInfo) string { return "" }

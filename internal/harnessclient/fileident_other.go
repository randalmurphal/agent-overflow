//go:build !unix

package harnessclient

import "os"

// FileIdentity has no cheap answer off unix: Windows would need a
// GetFileInformationByHandle round trip on an open handle, which is a
// syscall and a handle per file per check for a heuristic the size
// comparison already covers in the common case. Empty means "no
// identity", and every caller falls back to the size heuristic alone.
func FileIdentity(os.FileInfo) string { return "" }

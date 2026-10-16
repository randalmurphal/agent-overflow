//go:build !unix

package app

import "os"

// providerBinaryFileID is empty off unix, where reading a file id needs an
// open handle; path, size and mtime remain the identity there.
type providerBinaryFileID struct{}

func binaryFileID(os.FileInfo) providerBinaryFileID { return providerBinaryFileID{} }

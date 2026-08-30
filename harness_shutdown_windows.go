//go:build windows

package main

import "os"

// Windows has no portable self-SIGTERM equivalent. App.Shutdown has already
// drained the transport and stores, so exiting here leaves only discovery
// artifacts for the next list/prune pass.
func terminateSelf() error {
	os.Exit(0)
	return nil
}

//go:build !unix

package main

import "os"

// tryLockWriterFile: the mock runs its isolated harness on unix; elsewhere
// no lock is modeled and every load succeeds.
func tryLockWriterFile(*os.File) (bool, error) {
	return true, nil
}

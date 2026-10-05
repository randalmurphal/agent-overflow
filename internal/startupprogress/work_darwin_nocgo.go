//go:build darwin && !cgo

package startupprogress

import (
	"log"
	"sync"
	"syscall"
)

// storageIO reports no I/O counter. The disk counters come from libproc,
// which needs cgo, and getrusage's block counts stay at zero for file I/O
// on macOS.
func storageIO(*syscall.Rusage) (int64, string) {
	noDiskIO.Do(func() {
		log.Printf("startup progress: built without cgo, so storage I/O does not count as progress")
	})
	return 0, ""
}

// noDiskIO logs the missing I/O counter once.
var noDiskIO sync.Once

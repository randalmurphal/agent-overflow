//go:build darwin && cgo

package startupprogress

/*
#include <libproc.h>
#include <sys/resource.h>
#include <unistd.h>

static int ao_diskio(uint64_t *bytes) {
	struct rusage_info_v2 ri;
	if (proc_pid_rusage(getpid(), RUSAGE_INFO_V2, (rusage_info_t *)&ri) != 0) return -1;
	*bytes = ri.ri_diskio_bytesread + ri.ri_diskio_byteswritten;
	return 0;
}
*/
import "C"

import (
	"log"
	"sync"
	"syscall"
)

// storageIO is the bytes this process read from and wrote to disk, from
// proc_pid_rusage. Buffered writes count when they are flushed, including
// by the kernel's delayed writeback. getrusage's block counts are not used:
// macOS leaves them at zero for file I/O.
func storageIO(*syscall.Rusage) (int64, string) {
	var n C.uint64_t
	if r, err := C.ao_diskio(&n); r != 0 {
		diskIOFailure.Do(func() {
			log.Printf("startup progress: proc_pid_rusage failed, so storage I/O does not count as progress: %v", err)
		})
		return 0, ""
	}
	return int64(n), "proc_pid_rusage"
}

// diskIOFailure logs a proc_pid_rusage failure once.
var diskIOFailure sync.Once

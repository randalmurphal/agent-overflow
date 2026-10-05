//go:build !windows

package startupprogress

import (
	"fmt"
	"syscall"
	"time"
)

// ReadProcessWork samples this process's CPU time (getrusage) and its
// storage I/O from the platform's counter (storageIO).
func ReadProcessWork() (ProcessWork, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return ProcessWork{}, fmt.Errorf("getrusage: %w", err)
	}
	w := ProcessWork{CPU: time.Duration(ru.Utime.Nano() + ru.Stime.Nano())}
	w.IO, w.IOSource = storageIO(&ru)
	return w, nil
}

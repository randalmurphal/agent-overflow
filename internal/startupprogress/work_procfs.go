//go:build !windows && !darwin

package startupprogress

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
)

// storageIO is this process's storage I/O from /proc/self/io where it can
// be read (Linux), in bytes, and otherwise from getrusage's block counts,
// taken as 512-byte blocks. A Linux kernel without /proc/self/io keeps no
// per-process I/O accounting, which getrusage's block counts also come
// from, so I/O goes unmeasured.
func storageIO(ru *syscall.Rusage) (int64, string) {
	n, err := readProcSelfIO()
	switch {
	case err == nil:
		return n, "/proc/self/io"
	case errors.Is(err, fs.ErrNotExist) && runtime.GOOS == "linux":
		procIOFailure.Do(func() {
			log.Printf("startup progress: this kernel keeps no per-process I/O accounting, so storage I/O does not count as progress")
		})
		return 0, ""
	case !errors.Is(err, fs.ErrNotExist):
		procIOFailure.Do(func() {
			log.Printf("startup progress: cannot read /proc/self/io, counting I/O from getrusage: %v", err)
		})
	}
	return (int64(ru.Inblock) + int64(ru.Oublock)) * 512, "getrusage"
}

// procIOFailure logs once that /proc/self/io is missing or unreadable.
var procIOFailure sync.Once

// readProcSelfIO is read_bytes plus write_bytes from /proc/self/io: the
// bytes this process fetched from and sent toward storage.
func readProcSelfIO() (int64, error) {
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return 0, err
	}
	var total int64
	found := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		name, value, ok := bytes.Cut(sc.Bytes(), []byte(":"))
		if !ok || (string(name) != "read_bytes" && string(name) != "write_bytes") {
			continue
		}
		n, err := strconv.ParseInt(string(bytes.TrimSpace(value)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/self/io %s: %w", name, err)
		}
		total += n
		found++
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("/proc/self/io: %w", err)
	}
	if found != 2 {
		return 0, errors.New("/proc/self/io lacks read_bytes or write_bytes")
	}
	return total, nil
}

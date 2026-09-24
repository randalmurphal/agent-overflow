//go:build !windows

package transport

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// readProcessWork samples this process's CPU time and storage I/O. I/O
// comes from /proc/self/io where it can be read (Linux), in bytes, and
// otherwise from getrusage's block counts, taken as 512-byte blocks.
func readProcessWork() (processWork, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return processWork{}, fmt.Errorf("getrusage: %w", err)
	}
	w := processWork{cpu: time.Duration(ru.Utime.Nano() + ru.Stime.Nano())}
	if n, err := readProcSelfIO(); err == nil {
		w.io, w.ioSource = n, "/proc/self/io"
		return w, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		procIOFailure.Do(func() {
			log.Printf("startup progress: cannot read /proc/self/io, counting I/O from getrusage: %v", err)
		})
	}
	w.io, w.ioSource = (int64(ru.Inblock)+int64(ru.Oublock))*512, "getrusage"
	return w, nil
}

// procIOFailure logs an unexpected /proc/self/io failure once.
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

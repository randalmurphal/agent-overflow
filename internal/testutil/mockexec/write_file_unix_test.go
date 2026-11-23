//go:build !windows

package mockexec

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// The write goes to a FIFO the test reads, so it cannot close the file until
// the test drains it: while the test holds the read, the file is open for
// writing, and a process start must not be able to begin. Not parallel: it
// takes the process-wide fork lock.
func TestWriteFileHoldsForksWhileTheFileIsOpenForWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// Larger than a pipe's buffer, so the write blocks until the test reads.
	data := bytes.Repeat([]byte("x"), 1<<20)
	done := make(chan error, 1)
	go func() { done <- writeFile(path, data, 0o755) }()
	reader, err := os.Open(path) // returns once the writer has opened it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOnce(t, reader) })
	if syscall.ForkLock.TryLock() {
		syscall.ForkLock.Unlock()
		t.Fatal("a process could start while the executable was open for writing")
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, %v; want %d", len(got), err, len(data))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !syscall.ForkLock.TryLock() {
		t.Fatal("process starts were still held off after the write")
	}
	syscall.ForkLock.Unlock()
}

// The copy reads its source from a FIFO the test writes, so it cannot
// finish while the test holds the writing end open. Once the copy has
// created its destination, a process start must not be able to begin. Not
// parallel: it takes the process-wide fork lock.
func TestCopyFileHoldsForksWhileTheCopyIsOpenForWriting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := syscall.Mkfifo(src, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "copy")
	done := make(chan error, 1)
	go func() { done <- copyFile(path, src, 0o755) }()
	writer, err := os.OpenFile(src, os.O_WRONLY, 0) // returns once the copy has opened it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOnce(t, writer) })
	if _, err := writer.Write([]byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	// The copy's first read returns those bytes only after it has created
	// path, and it cannot close path until the writer closes.
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == "#!/bin/sh\n" {
			break
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("the copy ended before its source did: %v", err)
		default:
		}
		runtime.Gosched()
	}
	if syscall.ForkLock.TryLock() {
		syscall.ForkLock.Unlock()
		t.Fatal("a process could start while the copy was open for writing")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !syscall.ForkLock.TryLock() {
		t.Fatal("process starts were still held off after the copy")
	}
	syscall.ForkLock.Unlock()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("copy mode = %v, %v; want executable", info, err)
	}
}

// closeOnce closes file unless the test already has.
func closeOnce(t *testing.T, file *os.File) {
	if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close %s: %v", file.Name(), err)
	}
}

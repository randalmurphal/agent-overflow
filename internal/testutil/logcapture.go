package main

import (
	"bytes"
	"log"
	"sync"
	"testing"
)

// syncLogBuffer is a mutex-guarded sink for log.SetOutput in tests. Test
// bodies read the captured text while detached goroutines the code under test
// spawned (wake notifications, feedback acks, off-wire completions) may still
// be logging, so an unguarded bytes.Buffer is a data race under -race.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncLogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// captureLogOutput routes the standard logger into a race-safe buffer for the
// duration of the test.
func captureLogOutput(t *testing.T) *syncLogBuffer {
	t.Helper()
	logs := &syncLogBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	return logs
}

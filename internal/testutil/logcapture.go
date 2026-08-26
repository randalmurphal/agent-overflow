package testutil

import (
	"bytes"
	"log"
	"sync"
	"testing"
)

// SyncLogBuffer is a mutex-guarded sink for log.SetOutput in tests. Test
// bodies read the captured text while detached goroutines the code under test
// spawned (wake notifications, feedback acks, off-wire completions) may still
// be logging, so an unguarded bytes.Buffer is a data race under -race.
type SyncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SyncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *SyncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *SyncLogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// CaptureLogOutput routes the standard logger into a race-safe buffer for the
// duration of the test.
func CaptureLogOutput(t *testing.T) *SyncLogBuffer {
	t.Helper()
	logs := &SyncLogBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	return logs
}

package procutil

import (
	"syscall"
	"testing"
)

// Not parallel: it takes the process-wide fork lock.
func TestHoldForksBlocksAForkUntilReleased(t *testing.T) {
	release := HoldForks()
	if syscall.ForkLock.TryLock() {
		syscall.ForkLock.Unlock()
		release()
		t.Fatal("a fork could start while forks were held off")
	}
	second := HoldForks()
	second()
	release()
	if !syscall.ForkLock.TryLock() {
		t.Fatal("a fork still could not start after the release")
	}
	syscall.ForkLock.Unlock()
}

package worktreesetup

import "testing"

// Copying a large executable must not hold every process start in the app
// for the whole copy: the copy writes the file first and only then waits
// for the starts in progress, after which none of them holds it open.
func TestCopyWritesAnExecutableBeforeItWaitsForForks(t *testing.T) {
	waited, writtenFirst, _ := copyWhileAForkHoldsTheLockWritten(t, 0o755)
	if !waited {
		t.Fatal("the copy did not wait for the process start in progress")
	}
	if !writtenFirst {
		t.Fatal("the copy held process starts off before it wrote the file")
	}
}

//go:build !windows

package helperproc

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// EndGroup sends SIGKILL to process group pgid and waits until no member of
// it is left. It is for a group a test caused but another process started,
// such as the orphan reaper sidecar of a backend the test killed, which
// outlives that test unless it is ended. A member is gone once its parent
// reaps it, so the parent must not be this process. A group already gone
// is fine.
func EndGroup(t testing.TB, pgid int) {
	t.Helper()
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("end process group %d: %v", pgid, err)
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for syscall.Kill(-pgid, 0) == nil {
		if time.Now().After(deadline) {
			t.Errorf("process group %d outlived its SIGKILL", pgid)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

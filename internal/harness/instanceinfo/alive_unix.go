//go:build !windows

package instanceinfo

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ProcessAlive reports whether pid names a live process, using signal 0
// — the standard "check, don't deliver" probe. EPERM counts as alive:
// the process exists, it just belongs to another user, which is a fact
// about ownership rather than about liveness.
//
// Racy by nature (a pid can die between the probe and the caller's next
// line) and that is fine: the registry uses it to mark stale rows, and
// a row that goes stale a millisecond later is still stale.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	// Linux signal 0 also succeeds for a zombie until its parent reaps it.
	// A zombie cannot execute shutdown or own live work, so treating it as
	// alive makes group teardown mistake an exited leader for a PID-reuse
	// hazard and strand its surviving descendants.
	if runtime.GOOS == "linux" {
		data, statErr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if statErr == nil {
			if close := strings.LastIndexByte(string(data), ')'); close >= 0 {
				fields := strings.Fields(string(data)[close+1:])
				if len(fields) > 0 {
					switch fields[0] {
					case "Z", "X", "x":
						return false
					}
				}
			}
		}
	}
	return err == nil || errors.Is(err, syscall.EPERM)
}

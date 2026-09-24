package supervise

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnvInheritedLock tells a trial which descriptor carries the data root's
// backend lock. The parent holds the lock for the whole update; the trial
// inherits a descriptor on the same open file, so the data root stays locked
// until the trial exits even when the parent dies first.
const EnvInheritedLock = "AO_BACKEND_LOCK_FD"

// InheritedLockFD follows the channel's two descriptors.
const InheritedLockFD = 5

// AdoptInheritedLock returns the lock a trial inherited, or nil when there is
// none. The descriptor is made close-on-exec, so nothing the trial starts
// keeps the data root locked after it exits. The caller keeps the file
// referenced for the life of the process: a collected *os.File closes its
// descriptor. A refused descriptor is left untouched and unwrapped.
func AdoptInheritedLock(lookupEnv func(string) (string, bool), unsetEnv func(string) error) (*os.File, error) {
	value, ok := lookupEnv(EnvInheritedLock)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, nil
	}
	if unsetEnv != nil {
		if err := unsetEnv(EnvInheritedLock); err != nil {
			return nil, fmt.Errorf("supervise: clear %s: %w", EnvInheritedLock, err)
		}
	}
	fd, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || fd < 0 {
		return nil, fmt.Errorf("supervise: %s = %q is not a descriptor", EnvInheritedLock, value)
	}
	mode, err := descriptorMode(fd)
	if err != nil {
		return nil, fmt.Errorf("supervise: inherited lock descriptor %d: %w", fd, err)
	}
	if !mode.IsRegular() {
		return nil, fmt.Errorf("supervise: inherited lock descriptor %d is not a file", fd)
	}
	if err := setCloseOnExec(fd); err != nil {
		return nil, fmt.Errorf("supervise: inherited lock descriptor %d: %w", fd, err)
	}
	return os.NewFile(uintptr(fd), "backend-lock"), nil
}

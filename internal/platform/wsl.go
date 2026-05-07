package platform

import (
	"bytes"
	"os"
	"sync"
)

// WSLOSReleasePath is the kernel-reported runtime identifier on Linux. On WSL
// this string contains "microsoft" (case-insensitive); on native Linux it does
// not.
const WSLOSReleasePath = "/proc/sys/kernel/osrelease"

var (
	wslDetectionOnce  sync.Once
	wslDetectionValue bool
)

// IsWSL reports whether the running process is inside a WSL distribution. The
// result is cached after the first call.
func IsWSL() bool {
	wslDetectionOnce.Do(func() {
		wslDetectionValue = IsWSLFromOSRelease(os.ReadFile)
	})
	return wslDetectionValue
}

// IsWSLFromOSRelease is the test-friendly WSL detector. It reads through the
// supplied function so callers can inject fixtures without touching /proc.
func IsWSLFromOSRelease(readFile func(string) ([]byte, error)) bool {
	if readFile == nil {
		return IsWSL()
	}
	data, err := readFile(WSLOSReleasePath)
	if err != nil {
		return false
	}
	return bytes.Contains(bytes.ToLower(data), []byte("microsoft"))
}

//go:build !linux && !darwin && !windows

package supervise

import (
	"context"
	"errors"
	"time"
)

var errProcessRefUnsupported = errors.New("process start times are not readable on this platform")

func processStart(int) (string, bool, error) { return "", false, errProcessRefUnsupported }

func waitForExit(context.Context, ProcessRef, time.Duration) error {
	return errProcessRefUnsupported
}

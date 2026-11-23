//go:build windows

package orphanreaper

import (
	"errors"
	"time"
)

// errNoGroups: the desktop binary never spawns provider children on Windows
// (the WSL-side Linux backend owns them, and the launcher uses a Win32 Job
// Object for lifetime management). These exist so the package compiles for
// Windows.
var errNoGroups = errors.New("process groups are not signalled on windows")

func stopGroup(int, time.Duration) error { return errNoGroups }

func probeGroup(int) error { return errNoGroups }

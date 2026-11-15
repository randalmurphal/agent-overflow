package app

import "sync"

// LaunchSetting is a setting that applies when the app next starts: its
// saved value and the value this backend started with. A difference is a
// change waiting on a restart, so a page opened at any time, from any client,
// can say so.
type LaunchSetting struct {
	// Value is the saved setting.
	Value bool `json:"value"`
	// Launched is the value this backend started with.
	Launched bool `json:"launched"`
}

// launchValue holds a setting's value at this backend's start, recorded once.
type launchValue struct {
	mu    sync.Mutex
	known bool
	value bool
}

// record returns the recorded start value, taking v as it when none is
// recorded yet. The owner calls it with a value it knows the start used.
func (l *launchValue) record(v bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.known {
		l.known = true
		l.value = v
	}
	return l.value
}

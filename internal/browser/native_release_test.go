//go:build linux || darwin

package browser

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/uicall"
)

// A release the loop has not run yet is queued, not lost, and logs
// nothing; one whose loop is gone is logged once at its site.
func TestReleaseSiteLogsOnlyALostRelease(t *testing.T) {
	var (
		mu     sync.Mutex
		logged []string
		alive  = true
		queue  []func()
	)
	site := &releaseSite{what: "a test reference", logf: func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}}
	held := uicall.NewThread(func(fn func()) {
		mu.Lock()
		queue = append(queue, fn)
		mu.Unlock()
	}, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return alive
	}, time.Millisecond)

	ran := make(chan struct{}, 1)
	site.release(held, func() { ran <- struct{}{} })
	mu.Lock()
	pending, lines := queue, len(logged)
	queue = nil
	mu.Unlock()
	if lines != 0 || len(pending) != 1 {
		t.Fatalf("a queued release logged %d lines and queued %d", lines, len(pending))
	}
	pending[0]()
	<-ran

	inline := uicall.NewThread(func(fn func()) { fn() }, func() bool { return true }, time.Second)
	site.release(inline, func() { ran <- struct{}{} })
	<-ran

	mu.Lock()
	alive = false
	mu.Unlock()
	site.release(held, func() { t.Error("a release ran with its loop gone") })
	site.release(held, func() { t.Error("a release ran with its loop gone") })
	mu.Lock()
	defer mu.Unlock()
	if len(queue) != 0 {
		t.Fatalf("a release was posted to a loop that is gone: %d", len(queue))
	}
	if len(logged) != 1 || logged[0] != "browser: the UI loop is gone, so a test reference is not released" {
		t.Fatalf("lost releases logged %q", logged)
	}
}

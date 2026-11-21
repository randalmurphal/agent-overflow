//go:build linux || darwin

package browser

import (
	"log"
	"sync"

	"agent-overflow/internal/browser/uicall"
)

// releaseSite is one place a native engine gives up something it holds on
// the UI thread (uicall.Thread.Release), named by what it releases. A
// release still queued past the thread's timeout runs later, which is not
// a failure. One whose loop is gone never runs, and the first such at the
// site is logged with what was not released; the rest of the process's
// releases there go the same way and add nothing.
type releaseSite struct {
	what string
	// logf reports the lost release; log.Printf when nil.
	logf func(format string, args ...any)
	once sync.Once
}

// The release sites both native engines have.
var (
	releasePolicyAnswer = &releaseSite{what: "a navigation decision's answer"}
	releaseDestinations = &releaseSite{what: "the downloads a closed profile was still deciding"}
	releaseSiteData     = &releaseSite{what: "a closed profile's site-data store"}
	releaseDownloadRef  = &releaseSite{what: "a finished download's reference"}
)

func (s *releaseSite) release(thread *uicall.Thread, action func()) {
	if thread.Release(action) || thread.Alive() {
		return
	}
	s.once.Do(func() {
		logf := s.logf
		if logf == nil {
			logf = log.Printf
		}
		logf("browser: the UI loop is gone, so %s is not released", s.what)
	})
}

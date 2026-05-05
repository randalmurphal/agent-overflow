package design

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/rjeczalik/notify"
)

const (
	// watchDebounceWindow coalesces fs events into one frontend reload.
	// Mirrors internal/gitwatch's 250ms trailing-edge — agents often save
	// 3-5 files in one turn (Edit on multiple files, Write on a new
	// CSS/JS sibling); coalescing keeps the iframe from thrashing.
	watchDebounceWindow = 250 * time.Millisecond

	// watchDebounceMaxWait caps how long a continuous burst can keep
	// deferring an emit. Without this bound, a turn that streams writes
	// every <250ms would never broadcast. 1.5s matches gitwatch's
	// liveness/coalesce trade-off.
	watchDebounceMaxWait = 1500 * time.Millisecond

	// watchPollFallbackInterval is the cadence used when the recursive
	// fs watcher cannot be installed. Slow enough not to thrash, fast
	// enough that a real edit lands in a few seconds.
	watchPollFallbackInterval = 3 * time.Second

	// watchNotifyChannelSize bounds the per-watcher event queue. Sized
	// for a typical agent turn's burst.
	watchNotifyChannelSize = 64
)

// WatchSubject classifies which subtree of the per-thread working
// directory changed. The frontend dispatches different events for
// each — main reloads the iframe, options refreshes the options
// panel, snapshots refreshes the snapshot list.
type WatchSubject string

const (
	// WatchSubjectMain corresponds to a change inside main/.
	WatchSubjectMain WatchSubject = "main"
	// WatchSubjectOptions corresponds to a change inside options/.
	WatchSubjectOptions WatchSubject = "options"
	// WatchSubjectSnapshots corresponds to a change inside snapshots/.
	// Currently emitted only by snapshot creation/restore inside this
	// process, but watching the dir keeps us honest if anything else
	// touches it.
	WatchSubjectSnapshots WatchSubject = "snapshots"
)

// WatchEvent is one debounced summary of fs activity for a thread.
// SetID is populated only for WatchSubjectOptions events.
type WatchEvent struct {
	ThreadID string
	Subject  WatchSubject
	SetID    string
}

// Watcher fans WatchEvents out to a single subscriber. One instance
// per active design thread. NewWatcher is the only allocation point;
// callers Stop before allocating a replacement.
type Watcher struct {
	threadID string
	threadDir string
	emit     func(WatchEvent)

	ctx    context.Context
	cancel context.CancelFunc

	eventsCh chan notify.EventInfo
	done     chan struct{}

	fallbackPolling bool

	// installFn lets tests force the polling-fallback path. nil →
	// production install via notify.Watch.
	installFn func(dir string, ch chan<- notify.EventInfo) error
}

// WatcherOptions configure a watcher's optional seams.
type WatcherOptions struct {
	// InstallFn replaces the production notify.Watch installer. Tests
	// pass a stub that always returns an error so the polling fallback
	// runs on a deterministic schedule. Production leaves this nil.
	InstallFn func(dir string, ch chan<- notify.EventInfo) error
}

// NewWatcher constructs a watcher over threadDir and starts its run
// goroutine. emit is called from the run goroutine — implementations
// must not block it for long. Returned watcher's Stop blocks until the
// run goroutine exits.
func NewWatcher(threadID, threadDir string, emit func(WatchEvent), opts WatcherOptions) *Watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		threadID:  threadID,
		threadDir: threadDir,
		emit:      emit,
		ctx:       ctx,
		cancel:    cancel,
		eventsCh:  make(chan notify.EventInfo, watchNotifyChannelSize),
		done:      make(chan struct{}),
		installFn: opts.InstallFn,
	}
	w.start()
	return w
}

func (w *Watcher) start() {
	install := w.installFn
	if install == nil {
		install = installDesignNotifyWatcher
	}
	if err := install(w.threadDir, w.eventsCh); err != nil {
		log.Printf("design: fs watch unavailable for %s (%v); falling back to %s polling",
			w.threadDir, err, watchPollFallbackInterval)
		w.fallbackPolling = true
	}
	go w.run()
}

// Stop cancels the run loop and unregisters the fs watcher. Blocks
// until run exits.
func (w *Watcher) Stop() {
	w.cancel()
	notify.Stop(w.eventsCh)
	<-w.done
}

func installDesignNotifyWatcher(dir string, ch chan<- notify.EventInfo) error {
	return notify.Watch(filepath.Join(dir, "..."), ch, notify.All)
}

// pendingEvent tracks the coalesced subjects + option set ids accumulated
// during a debounce window. Multiple subdirs touched in one burst emit
// once per (subject, setID) pair on debounce fire — that mirrors how
// the frontend wants to react: one panel update per affected surface.
type pendingEvent struct {
	subjects map[WatchSubject]map[string]struct{}
}

func newPendingEvent() *pendingEvent {
	return &pendingEvent{subjects: make(map[WatchSubject]map[string]struct{})}
}

func (p *pendingEvent) add(subject WatchSubject, setID string) {
	bySet, ok := p.subjects[subject]
	if !ok {
		bySet = make(map[string]struct{})
		p.subjects[subject] = bySet
	}
	bySet[setID] = struct{}{}
}

func (p *pendingEvent) drain(emit func(WatchSubject, string)) {
	for subject, sets := range p.subjects {
		for setID := range sets {
			emit(subject, setID)
		}
	}
}

func (w *Watcher) run() {
	defer close(w.done)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("design watcher: panic for %s: %v", w.threadID, r)
		}
	}()

	debounce := time.NewTimer(watchDebounceWindow)
	if !debounce.Stop() {
		<-debounce.C
	}
	debounceArmed := false
	var firstEventAt time.Time
	pending := newPendingEvent()

	var pollCh <-chan time.Time
	if w.fallbackPolling {
		ticker := time.NewTicker(watchPollFallbackInterval)
		defer ticker.Stop()
		pollCh = ticker.C
	}

	flush := func() {
		debounceArmed = false
		drained := pending
		pending = newPendingEvent()
		drained.drain(func(subject WatchSubject, setID string) {
			w.emit(WatchEvent{
				ThreadID: w.threadID,
				Subject:  subject,
				SetID:    setID,
			})
		})
	}

	for {
		select {
		case <-w.ctx.Done():
			return
		case ev, ok := <-w.eventsCh:
			if !ok {
				return
			}
			subject, setID, accept := classifyEvent(w.threadDir, ev.Path())
			if !accept {
				continue
			}
			pending.add(subject, setID)
			drainPendingEvents(w.eventsCh, w.threadDir, pending)

			now := time.Now()
			if !debounceArmed {
				firstEventAt = now
			}
			if debounceArmed && now.Sub(firstEventAt) >= watchDebounceMaxWait {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				flush()
				continue
			}
			if debounceArmed {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
			}
			debounce.Reset(watchDebounceWindow)
			debounceArmed = true
		case <-debounce.C:
			flush()
		case <-pollCh:
			// On polling fallback we can't tell which subdir changed
			// without a stat scan. Emit a Main event — the most common
			// case — and let the frontend's deduplication handle the
			// occasional spurious reload. Polling fallback is rare; the
			// trade-off keeps the watcher dirt simple.
			w.emit(WatchEvent{ThreadID: w.threadID, Subject: WatchSubjectMain})
		}
	}
}

func drainPendingEvents(ch chan notify.EventInfo, threadDir string, pending *pendingEvent) {
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			subject, setID, accept := classifyEvent(threadDir, ev.Path())
			if !accept {
				continue
			}
			pending.add(subject, setID)
		default:
			return
		}
	}
}

// classifyEvent maps an absolute path inside threadDir to its subject.
// Returns (subject, setID, true) for accepted events. Paths outside the
// thread tree, or ones that look like our own atomic-write tmp files,
// are rejected to keep the agent's writes from triggering self-events
// during snapshot/restore.
func classifyEvent(threadDir, abs string) (WatchSubject, string, bool) {
	threadDir = filepath.Clean(threadDir)
	abs = filepath.Clean(abs)
	if !strings.HasPrefix(abs, threadDir+string(filepath.Separator)) {
		return "", "", false
	}
	rel := strings.TrimPrefix(abs, threadDir+string(filepath.Separator))
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 {
		return "", "", false
	}
	// Suppress events from our own tmp-rename writes. The watcher loop
	// would otherwise see the .tmp create/write before the rename and
	// re-emit, doubling reload counts during heavy iteration. We have
	// to scan ALL segments because copyTreeAtomic stages files under
	// `snapshots/{snapID}.tmp-{uuid}/...` — only the parent directory
	// segment carries the tmp marker; the file inside it (`index.html`)
	// looks innocuous on its own. Same for `restore-` rollbacks.
	for _, p := range parts {
		if isTmpSegment(p) {
			return "", "", false
		}
	}
	switch parts[0] {
	case subdirMain:
		return WatchSubjectMain, "", true
	case subdirOptions:
		setID := ""
		if len(parts) >= 2 {
			setID = parts[1]
		}
		return WatchSubjectOptions, setID, true
	case subdirSnapshots:
		return WatchSubjectSnapshots, "", true
	default:
		return "", "", false
	}
}

// isTmpSegment matches one path component against the atomic-write
// markers Workdir uses. Anchored prefix/suffix patterns instead of
// substring matches: `theme.tmp-dark.css` is a real CSS file the agent
// might write, not one of our staging dirs.
func isTmpSegment(part string) bool {
	if part == "" {
		return false
	}
	if strings.HasSuffix(part, ".tmp") {
		return true
	}
	// copyTreeAtomic stages directories as `<final>.tmp-<uuid>` and
	// rename-aside as `<final>.old-<uuid>`. RestoreFromSnapshot uses
	// `main.restore-<uuid>` similarly. Anchor the markers as the start
	// of the trailing suffix (after the final `.`) so user-named files
	// like `theme.tmp-dark.css` aren't suppressed.
	for _, marker := range tmpSegmentMarkers {
		idx := strings.LastIndex(part, marker)
		if idx < 0 {
			continue
		}
		// Marker must be followed by hex/uuid-ish chars only — reject
		// cases like `theme.tmp-dark.css` (extension after the marker
		// content).
		rest := part[idx+len(marker):]
		if rest == "" {
			return true
		}
		if isUUIDLike(rest) {
			return true
		}
	}
	return false
}

var tmpSegmentMarkers = []string{".tmp-", ".old-", ".restore-"}

// isUUIDLike reports whether s consists only of hex digits and dashes
// (the shape uuid.NewString produces). Tolerant — any subset works
// because workdir.go formats are stable; the goal is to reject
// human-named extensions like "dark.css".
func isUUIDLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

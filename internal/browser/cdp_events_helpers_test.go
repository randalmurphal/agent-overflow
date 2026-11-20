package browser

import (
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// eventStep is one CDP event fed to a handler, named for failure messages.
type eventStep struct {
	name string
	run  func()
}

// interleavings returns every merge of streams that keeps each stream's own
// order: the orders chromedp can hand per-method subscriptions to their
// handlers in, whatever order the browser sent the events in.
func interleavings(streams [][]eventStep) [][]eventStep {
	var out [][]eventStep
	var walk func(prefix []eventStep, rest [][]eventStep)
	walk = func(prefix []eventStep, rest [][]eventStep) {
		done := true
		for i, stream := range rest {
			if len(stream) == 0 {
				continue
			}
			done = false
			next := make([][]eventStep, len(rest))
			copy(next, rest)
			next[i] = stream[1:]
			walk(append(append([]eventStep(nil), prefix...), stream[0]), next)
		}
		if done {
			out = append(out, prefix)
		}
	}
	walk(nil, streams)
	return out
}

func stepNames(steps []eventStep) string {
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = step.name
	}
	return strings.Join(names, ", ")
}

// downloadRecorder stands in for the Manager's download bookkeeping: like
// downloadProgress, it ignores progress for a download it never saw start.
type downloadRecorder struct {
	mu       sync.Mutex
	allow    bool
	state    map[downloadID]string
	bytes    map[downloadID]float64
	canceled map[downloadID]int
}

func newDownloadRecorder(allow bool) *downloadRecorder {
	return &downloadRecorder{allow: allow, state: map[downloadID]string{}, bytes: map[downloadID]float64{}, canceled: map[downloadID]int{}}
}

func (r *downloadRecorder) events() engineEvents {
	return engineEvents{
		DownloadStarted: func(start downloadStart) downloadAdmission {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.state[start.ID] = downloadInProgress.String()
			if !r.allow {
				r.state[start.ID] = "refused"
				return downloadRefused
			}
			return downloadAdmitted
		},
		DownloadProgress: func(progress downloadProgress) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if state, ok := r.state[progress.ID]; !ok || state == "refused" {
				return
			}
			r.state[progress.ID] = progress.State.String()
			r.bytes[progress.ID] = progress.Received
		},
	}
}

func (r *downloadRecorder) cancel(id downloadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canceled[id]++
}

func (r *downloadRecorder) outcome(id downloadID) (string, float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state[id], r.bytes[id]
}

// popupRecorder stands in for the Manager's page events. Popups and closes
// are reported on goroutines of their own, so it is synchronized and waited
// on.
type popupRecorder struct {
	mu      sync.Mutex
	changed chan struct{}
	opened  map[string]enginePopup
	closed  map[string]int
	infos   map[string][]string
}

func newPopupRecorder() *popupRecorder {
	return &popupRecorder{changed: make(chan struct{}, 64), opened: map[string]enginePopup{}, closed: map[string]int{}, infos: map[string][]string{}}
}

func (r *popupRecorder) events() engineEvents {
	return engineEvents{
		PopupOpened: func(popup enginePopup) {
			r.mu.Lock()
			r.opened[string(popup.Handle)] = popup
			r.mu.Unlock()
			r.changed <- struct{}{}
		},
		PageClosed: func(handle ids.PageHandle) {
			r.mu.Lock()
			r.closed[string(handle)]++
			r.mu.Unlock()
			r.changed <- struct{}{}
		},
		PageInfoChanged: func(handle ids.PageHandle, url, title string, _ uint64) {
			r.mu.Lock()
			r.infos[string(handle)] = append(r.infos[string(handle)], url)
			r.mu.Unlock()
		},
	}
}

// waitFor waits until done reports true of the recorder.
func (r *popupRecorder) waitFor(t *testing.T, what string, done func(r *popupRecorder) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		ok := done(r)
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-r.changed:
		case <-deadline:
			t.Fatalf("never saw %s", what)
		}
	}
}

// newestURL is the URL the Manager ends with: the popup's own, then each
// info change after it.
func (r *popupRecorder) newestURL(handle string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if infos := r.infos[handle]; len(infos) > 0 {
		return infos[len(infos)-1]
	}
	return r.opened[handle].URL
}

func newTargetTestProfile(t *testing.T, events engineEvents) *headlessProfile {
	t.Helper()
	engine := &headlessEngine{
		events: events, logf: t.Logf,
		profiles: make(map[*headlessProfile]struct{}), pageProfile: make(map[ids.PageHandle]*headlessProfile),
	}
	p := &headlessProfile{engine: engine, handle: "profile"}
	engine.profiles[p] = struct{}{}
	return p
}

// fakeDownloadFiles stands in for a browser process's download directory,
// recording what the tracker did with each download's files, in order:
// "discard", "handOver", and "kept" for an empty that kept them.
type fakeDownloadFiles struct {
	mu      sync.Mutex
	log     map[downloadID][]string
	emptied int
	closed  int
}

func newFakeDownloadFiles() *fakeDownloadFiles {
	return &fakeDownloadFiles{log: map[downloadID][]string{}}
}

func (f *fakeDownloadFiles) note(id downloadID, what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log[id] = append(f.log[id], what)
}

func (f *fakeDownloadFiles) discard(id downloadID) { f.note(id, "discard") }

func (f *fakeDownloadFiles) handOver(id downloadID) (string, error) {
	f.note(id, "handOver")
	return "/downloads/" + string(id), nil
}

func (f *fakeDownloadFiles) withdraw(id downloadID) { f.note(id, "withdraw") }

func (f *fakeDownloadFiles) empty(keep map[downloadID]bool) {
	f.mu.Lock()
	f.emptied++
	f.mu.Unlock()
	for id := range keep {
		f.note(id, "kept")
	}
}

func (f *fakeDownloadFiles) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

// of is what was done with id's files, in order.
func (f *fakeDownloadFiles) of(id downloadID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log[id], " ")
}

// closes is how many times the directory was closed.
func (f *fakeDownloadFiles) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

package tasktail

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// newTailer builds a tailer whose ticker never fires inside a test; polls
// are driven by Poll.
func newTailer(t *testing.T, rec *recorder, tailBytes int) *Tailer {
	t.Helper()
	tailer := New(Config{Emit: rec.emit, Interval: time.Hour, TailBytes: tailBytes})
	t.Cleanup(tailer.Close)
	return tailer
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWatchSeedsTailAndStreamsAppends(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "line 1\n")

	snap, err := tailer.Watch("t1", "launch-1", path)
	if err != nil {
		t.Fatal(err)
	}
	if snap.WatchID == "" || snap.Data != "line 1\n" || snap.Truncated || snap.Ended || snap.Seq != 0 {
		t.Fatalf("seed snapshot: %+v", snap)
	}
	if len(rec.all()) != 0 {
		t.Fatalf("the seed read emits nothing; got %+v", rec.all())
	}

	tailer.Poll()
	if len(rec.all()) != 0 {
		t.Fatalf("an unchanged file emits nothing; got %+v", rec.all())
	}

	appendFile(t, path, "line 2\n")
	tailer.Poll()
	events := rec.all()
	if len(events) != 1 || events[0].Append != "line 2\n" || events[0].Seq != 1 || events[0].Reset || events[0].Truncated {
		t.Fatalf("append frame: %+v", events)
	}
	if events[0].ThreadID != "t1" || events[0].LaunchID != "launch-1" || events[0].Size != int64(len("line 1\nline 2\n")) {
		t.Fatalf("frame identity: %+v", events[0])
	}

	// A second watch on the same launch shares the follow and sees the
	// whole tail; its seq is the follow's.
	second, err := tailer.Watch("t1", "launch-1", path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Data != "line 1\nline 2\n" || second.Seq != 1 || second.WatchID == snap.WatchID {
		t.Fatalf("second snapshot: %+v", second)
	}
	if tailer.Watching() != 2 {
		t.Fatalf("watching %d, want 2", tailer.Watching())
	}

	// Releasing one watch keeps the follow; releasing the last drops it,
	// and a later release of either is a no-op.
	tailer.Release(snap.WatchID)
	appendFile(t, path, "line 3\n")
	tailer.Poll()
	if events := rec.all(); len(events) != 2 || events[1].Append != "line 3\n" {
		t.Fatalf("the surviving watch keeps the follow: %+v", events)
	}
	tailer.Release(second.WatchID)
	tailer.Release(second.WatchID)
	appendFile(t, path, "line 4\n")
	tailer.Poll()
	if events := rec.all(); len(events) != 2 {
		t.Fatalf("a released follow emits nothing: %+v", events)
	}
	if tailer.Watching() != 0 {
		t.Fatalf("watching %d, want 0", tailer.Watching())
	}
}

func TestSeedOfLargeFileKeepsTailFromLineBoundary(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, 64)
	path := filepath.Join(t.TempDir(), "task.output")
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("0123456789abcdef\n") // 17 bytes a line
	}
	appendFile(t, path, sb.String())

	snap, err := tailer.Watch("t1", "launch-1", path)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Truncated {
		t.Fatalf("a file past the tail bound must read truncated: %+v", snap)
	}
	// 64 bytes back from the end lands inside a line; the tail starts at
	// the next line: three whole lines of the twenty.
	if want := strings.Repeat("0123456789abcdef\n", 3); snap.Data != want {
		t.Fatalf("tail %q, want %q", snap.Data, want)
	}
}

func TestAppendPastBoundDropsHeadAtLineBoundary(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, 40)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "first line\n")
	if _, err := tailer.Watch("t1", "launch-1", path); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "second line here\n")
	appendFile(t, path, "third line here\n")
	tailer.Poll()
	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("one frame for one poll: %+v", events)
	}
	// The frame carries what arrived; the client bounds its own copy by
	// the same rule.
	if events[0].Append != "second line here\nthird line here\n" || !events[0].Truncated {
		t.Fatalf("append frame: %+v", events[0])
	}
	snap, err := tailer.Watch("t1", "launch-1", path)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Data != "second line here\nthird line here\n" || !snap.Truncated {
		t.Fatalf("bounded tail: %+v", snap)
	}
}

func TestGrowthPastBoundBetweenPollsResetsTail(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, 32)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "start\n")
	if _, err := tailer.Watch("t1", "launch-1", path); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, strings.Repeat("x", 100)+"\nlast line\n")
	tailer.Poll()
	events := rec.all()
	if len(events) != 1 || !events[0].Reset || !events[0].Truncated {
		t.Fatalf("a burst past the bound resets the tail: %+v", events)
	}
	if events[0].Append != "last line\n" {
		t.Fatalf("reset tail %q, want the last whole line", events[0].Append)
	}
	stats := tailer.Stats()
	if stats.BytesRead > int64(len("start\n")+32) {
		t.Fatalf("a burst reads at most the tail bound: %+v", stats)
	}
}

func TestShrunkFileIsReadAgainFromItsStart(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "one\ntwo\n")
	if _, err := tailer.Watch("t1", "launch-1", path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tailer.Poll()
	events := rec.all()
	if len(events) != 1 || !events[0].Reset || events[0].Append != "new\n" || events[0].Truncated {
		t.Fatalf("shrink frame: %+v", events)
	}
}

func TestSplitMultiByteCharacterWaitsForItsBytes(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "ok\n")
	if _, err := tailer.Watch("t1", "launch-1", path); err != nil {
		t.Fatal(err)
	}
	snowman := []byte("☃") // three bytes
	appendFile(t, path, "a"+string(snowman[:2]))
	tailer.Poll()
	appendFile(t, path, string(snowman[2:])+"\n")
	tailer.Poll()
	events := rec.all()
	if len(events) != 2 || events[0].Append != "a" || events[1].Append != "☃\n" {
		t.Fatalf("frames must carry whole characters: %+v", events)
	}
}

func TestFileNotYetWrittenIsPickedUpWhenItAppears(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	snap, err := tailer.Watch("t1", "launch-1", path)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Data != "" || snap.Ended || snap.Error != "" {
		t.Fatalf("a missing file is an empty tail, not an error: %+v", snap)
	}
	tailer.Poll()
	if len(rec.all()) != 0 {
		t.Fatalf("still missing, nothing emitted: %+v", rec.all())
	}
	appendFile(t, path, "hello\n")
	tailer.Poll()
	events := rec.all()
	if len(events) != 1 || events[0].Append != "hello\n" || events[0].Reset {
		t.Fatalf("first bytes of a late file: %+v", events)
	}
}

func TestResolveRefusalEndsTheWatchWithAnError(t *testing.T) {
	rec := &recorder{}
	tailer := New(Config{
		Emit:     rec.emit,
		Interval: time.Hour,
		Resolve: func(path string) (string, error) {
			return "", errors.New("output_file path is outside allowed roots")
		},
	})
	t.Cleanup(tailer.Close)
	snap, err := tailer.Watch("t1", "launch-1", "/elsewhere/task.output")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Ended || !strings.Contains(snap.Error, "outside allowed roots") {
		t.Fatalf("a refused path ends the watch in the snapshot: %+v", snap)
	}
	if stats := tailer.Stats(); stats.Stats != 0 || stats.Reads != 0 {
		t.Fatalf("a refused path is never read: %+v", stats)
	}
}

func TestResolveNotExistKeepsWaiting(t *testing.T) {
	rec := &recorder{}
	tailer := New(Config{
		Emit:     rec.emit,
		Interval: time.Hour,
		Resolve: func(path string) (string, error) {
			return "", fs.ErrNotExist
		},
	})
	t.Cleanup(tailer.Close)
	snap, err := tailer.Watch("t1", "launch-1", "/tmp/not-yet.output")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Ended || snap.Error != "" {
		t.Fatalf("not-exist from the guard is a wait, not an error: %+v", snap)
	}
}

func TestEndLaunchReadsTheLastBytesThenEnds(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "one\n")
	snap, err := tailer.Watch("t1", "launch-1", path)
	if err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "two\n")
	tailer.EndLaunch("t1", "launch-1")
	events := rec.all()
	if len(events) != 2 {
		t.Fatalf("a last read then the end: %+v", events)
	}
	if events[0].Append != "two\n" || events[0].Ended {
		t.Fatalf("last read: %+v", events[0])
	}
	if !events[1].Ended || events[1].Seq != 2 || events[1].Error != "" {
		t.Fatalf("end frame: %+v", events[1])
	}
	if tailer.Watching() != 0 {
		t.Fatalf("an ended launch keeps no watches, got %d", tailer.Watching())
	}
	// The watch id is gone; releasing it is a no-op, and another end is too.
	tailer.Release(snap.WatchID)
	tailer.EndLaunch("t1", "launch-1")
	appendFile(t, path, "three\n")
	tailer.Poll()
	if len(rec.all()) != 2 {
		t.Fatalf("nothing follows the end: %+v", rec.all())
	}
	// A new watch after the end starts a fresh follow of the file.
	fresh, err := tailer.Watch("t1", "launch-1", path)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Data != "one\ntwo\nthree\n" || fresh.Ended {
		t.Fatalf("fresh follow: %+v", fresh)
	}
}

func TestWatchCapAndClose(t *testing.T) {
	rec := &recorder{}
	tailer := New(Config{Emit: rec.emit, Interval: time.Hour, MaxWatches: 2})
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "x\n")
	if _, err := tailer.Watch("t1", "a", path); err != nil {
		t.Fatal(err)
	}
	if _, err := tailer.Watch("t1", "b", path); err != nil {
		t.Fatal(err)
	}
	if _, err := tailer.Watch("t1", "c", path); !errors.Is(err, ErrTooManyWatches) {
		t.Fatalf("third watch: %v, want ErrTooManyWatches", err)
	}
	tailer.Close()
	tailer.Close()
	if _, err := tailer.Watch("t1", "a", path); !errors.Is(err, ErrClosed) {
		t.Fatalf("watch after close: %v, want ErrClosed", err)
	}
	if tailer.Watching() != 0 {
		t.Fatalf("close drops every watch, got %d", tailer.Watching())
	}
}

// The cost of a follow per poll: an idle file is one fstat and no read;
// a growing file is one fstat and one pread of the new bytes.
func TestPollCostIdleAndBusy(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "seed\n")
	if _, err := tailer.Watch("t1", "launch-1", path); err != nil {
		t.Fatal(err)
	}
	base := tailer.Stats()
	for i := 0; i < 100; i++ {
		tailer.Poll()
	}
	idle := tailer.Stats()
	if got := idle.Stats - base.Stats; got != 100 {
		t.Fatalf("idle polls: %d stats, want 100", got)
	}
	if idle.Reads != base.Reads || idle.BytesRead != base.BytesRead {
		t.Fatalf("idle polls read nothing: %+v -> %+v", base, idle)
	}
	line := strings.Repeat("y", 79) + "\n"
	for i := 0; i < 100; i++ {
		appendFile(t, path, line)
		tailer.Poll()
	}
	busy := tailer.Stats()
	if got := busy.Stats - idle.Stats; got != 100 {
		t.Fatalf("busy polls: %d stats, want 100", got)
	}
	if got := busy.Reads - idle.Reads; got != 100 {
		t.Fatalf("busy polls: %d reads, want 100", got)
	}
	if got := busy.BytesRead - idle.BytesRead; got != int64(100*len(line)) {
		t.Fatalf("busy polls read the new bytes only: %d, want %d", got, 100*len(line))
	}
	t.Logf("idle: %d fstat, 0 reads per poll; busy: 1 fstat, 1 pread of %d bytes per poll", 1, len(line))
}

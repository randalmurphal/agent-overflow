package tasktail

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

	snap, err := tailer.Watch("t1", "launch-1", path, "c1")
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
	second, err := tailer.Watch("t1", "launch-1", path, "c1")
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
	tailer.Release("t1", snap.WatchID, "c1")
	appendFile(t, path, "line 3\n")
	tailer.Poll()
	if events := rec.all(); len(events) != 2 || events[1].Append != "line 3\n" {
		t.Fatalf("the surviving watch keeps the follow: %+v", events)
	}
	tailer.Release("t1", second.WatchID, "c1")
	tailer.Release("t1", second.WatchID, "c1")
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

	snap, err := tailer.Watch("t1", "launch-1", path, "c1")
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
	if _, err := tailer.Watch("t1", "launch-1", path, "c1"); err != nil {
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
	snap, err := tailer.Watch("t1", "launch-1", path, "c1")
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
	if _, err := tailer.Watch("t1", "launch-1", path, "c1"); err != nil {
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
	if _, err := tailer.Watch("t1", "launch-1", path, "c1"); err != nil {
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
	if _, err := tailer.Watch("t1", "launch-1", path, "c1"); err != nil {
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
	snap, err := tailer.Watch("t1", "launch-1", path, "c1")
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
	snap, err := tailer.Watch("t1", "launch-1", "/elsewhere/task.output", "c1")
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
	snap, err := tailer.Watch("t1", "launch-1", "/tmp/not-yet.output", "c1")
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
	snap, err := tailer.Watch("t1", "launch-1", path, "c1")
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
	tailer.Release("t1", snap.WatchID, "c1")
	tailer.EndLaunch("t1", "launch-1")
	appendFile(t, path, "three\n")
	tailer.Poll()
	if len(rec.all()) != 2 {
		t.Fatalf("nothing follows the end: %+v", rec.all())
	}
	// A new watch after the end starts a fresh follow of the file.
	fresh, err := tailer.Watch("t1", "launch-1", path, "c1")
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
	if _, err := tailer.Watch("t1", "a", path, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tailer.Watch("t1", "b", path, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tailer.Watch("t1", "c", path, "c1"); !errors.Is(err, ErrTooManyWatches) {
		t.Fatalf("third watch: %v, want ErrTooManyWatches", err)
	}
	tailer.Close()
	tailer.Close()
	if _, err := tailer.Watch("t1", "a", path, "c1"); !errors.Is(err, ErrClosed) {
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
	if _, err := tailer.Watch("t1", "launch-1", path, "c1"); err != nil {
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

// A watch joining a follow others hold reads what the file gained since
// their last poll; that read is their frame, not a silent skip.
func TestSecondWatchDeliversTheGapToExistingWatchers(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "line 1\n")
	first, err := tailer.Watch("t1", "launch-1", path, "c1")
	if err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "line 2\n")
	second, err := tailer.Watch("t1", "launch-1", path, "c2")
	if err != nil {
		t.Fatal(err)
	}
	events := rec.all()
	if len(events) != 1 || events[0].Append != "line 2\n" || events[0].Seq != 1 {
		t.Fatalf("the first watcher's frame for the bytes the second's seed read: %+v", events)
	}
	if second.Seq != 1 || second.Data != "line 1\nline 2\n" {
		t.Fatalf("second snapshot carries the same bytes at the frame's seq: %+v", second)
	}
	appendFile(t, path, "line 3\n")
	tailer.Poll()
	if events := rec.all(); len(events) != 2 || events[1].Seq != 2 || events[1].Append != "line 3\n" {
		t.Fatalf("later growth continues the seq: %+v", events)
	}
	tailer.Release("t1", first.WatchID, "c1")
	tailer.Release("t1", second.WatchID, "c2")
}

// A poll that captured a follow before its last watch was released
// reads nothing: the file it closed stays closed.
func TestReleaseMarksTheFollowSoAQueuedPollReadsNothing(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "line 1\n")
	snap, err := tailer.Watch("t1", "launch-1", path, "c1")
	if err != nil {
		t.Fatal(err)
	}
	tailer.mu.Lock()
	f := tailer.follows[followKey{thread: "t1", launch: "launch-1"}]
	tailer.mu.Unlock()
	tailer.Release("t1", snap.WatchID, "c1")
	appendFile(t, path, "line 2\n")
	before := tailer.Stats()
	f.mu.Lock()
	tailer.pollLocked(f, true)
	opened := f.file != nil
	f.mu.Unlock()
	if opened || tailer.Stats() != before || len(rec.all()) != 0 {
		t.Fatalf("a released follow must not reopen its file: opened=%v stats %+v -> %+v events %+v", opened, before, tailer.Stats(), rec.all())
	}
}

// A read that fails ends the follow and drops its watches, so the ids
// stop counting against the cap whether or not their clients release
// them.
func TestFailedReadDropsItsWatches(t *testing.T) {
	rec := &recorder{}
	tailer := New(Config{
		Emit: rec.emit, Interval: time.Hour, MaxWatches: 2,
		Resolve: func(path string) (string, error) {
			if strings.HasSuffix(path, "refused") {
				return "", errors.New("outside the allowed roots")
			}
			return path, nil
		},
	})
	t.Cleanup(tailer.Close)
	dir := t.TempDir()
	good := filepath.Join(dir, "task.output")
	appendFile(t, good, "x\n")

	// A seed that fails answers Ended and holds nothing.
	snap, err := tailer.Watch("t1", "a", filepath.Join(dir, "refused"), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Ended || snap.Error == "" || snap.WatchID != "" || tailer.Watching() != 0 {
		t.Fatalf("failed seed: %+v watching %d", snap, tailer.Watching())
	}

	// A follow that fails later drops its watches on that poll.
	first, err := tailer.Watch("t1", "b", good, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tailer.Watch("t1", "b", good, "c2"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(good); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(good, 0o755); err != nil {
		t.Fatal(err)
	}
	tailer.mu.Lock()
	f := tailer.follows[followKey{thread: "t1", launch: "b"}]
	tailer.mu.Unlock()
	f.mu.Lock()
	f.closeFileLocked()
	f.mu.Unlock()
	tailer.Poll()
	events := rec.all()
	if len(events) != 1 || !events[0].Ended || events[0].Error == "" {
		t.Fatalf("the failure frame: %+v", events)
	}
	if tailer.Watching() != 0 {
		t.Fatalf("a failed follow keeps no watches, got %d", tailer.Watching())
	}
	tailer.Release("t1", first.WatchID, "c1")
	if _, err := tailer.Watch("t1", "c", filepath.Join(dir, "other.output"), "c1"); err != nil {
		t.Fatalf("the cap has room again: %v", err)
	}
}

// Only the owner that opened a watch, naming its thread, releases it; the
// ids are not a sequence another client can walk.
func TestReleaseNeedsTheOwnerAndThread(t *testing.T) {
	rec := &recorder{}
	tailer := newTailer(t, rec, DefaultTailBytes)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "x\n")
	mine, err := tailer.Watch("t1", "launch-1", path, "c1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := tailer.Watch("t1", "launch-1", path, "c2")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{mine.WatchID, other.WatchID} {
		if len(id) < 20 {
			t.Fatalf("watch id %q is guessable", id)
		}
		if _, err := strconv.ParseUint(id, 10, 64); err == nil {
			t.Fatalf("watch id %q is a counter", id)
		}
	}
	tailer.Release("t1", other.WatchID, "c1")
	tailer.Release("t2", mine.WatchID, "c1")
	if tailer.Watching() != 2 {
		t.Fatalf("another owner's id, or the wrong thread, releases nothing; watching %d", tailer.Watching())
	}
	tailer.Release("t1", mine.WatchID, "c1")
	if tailer.Watching() != 1 {
		t.Fatalf("the owner's release lands; watching %d", tailer.Watching())
	}
	tailer.Release("t1", other.WatchID, "c2")
}

// Every byte cut lands on a character boundary: the tail bound inside a
// line, and the start of a burst read from inside the file.
func TestCutsLandOnCharacterBoundaries(t *testing.T) {
	// "é" is two bytes; a bound of 5 inside "ééé" (6 bytes, no newline)
	// lands between the bytes of the second character.
	if got := string(boundTail([]byte("ééé"), 5)); got != "éé" {
		t.Fatalf("boundTail inside a character: %q", got)
	}
	if got := string(afterFirstNewline([]byte("\xa9éx"))); got != "éx" {
		t.Fatalf("afterFirstNewline from a continuation byte: %q", got)
	}
	rec := &recorder{}
	tailer := newTailer(t, rec, 7)
	path := filepath.Join(t.TempDir(), "task.output")
	appendFile(t, path, "ab\n")
	if _, err := tailer.Watch("t1", "launch-1", path, "c1"); err != nil {
		t.Fatal(err)
	}
	// 20 bytes of two-byte characters with no newline arrive at once:
	// the read starts 7 bytes before the end, inside a character.
	appendFile(t, path, strings.Repeat("é", 10))
	tailer.Poll()
	events := rec.all()
	if len(events) != 1 || !events[0].Reset || !utf8.ValidString(events[0].Append) || events[0].Append != strings.Repeat("é", 3) {
		t.Fatalf("burst frame: %+v", events)
	}
}

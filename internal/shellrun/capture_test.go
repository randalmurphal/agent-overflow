package shellrun

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/procutil"
)

func newTestCapture(t *testing.T, rec *recorder) (*run, *capture) {
	t.Helper()
	current := &run{
		request: Request{ThreadID: "thread-1", RunID: "run-1"},
		tail:    procutil.NewTailBuffer(tailBytes),
	}
	sink, err := newCapture(filepath.Join(t.TempDir(), "run-1.log"), current, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	current.capture.Store(sink)
	t.Cleanup(func() {
		if err := sink.close(); err != nil {
			t.Errorf("close capture: %v", err)
		}
	})
	return current, sink
}

func framesThrough(rec *recorder, seq uint64) string {
	events, _, _ := rec.snapshot()
	var out strings.Builder
	for _, event := range events {
		if event.Phase == PhaseOutput && event.Seq <= seq {
			out.WriteString(event.Chunk)
		}
	}
	return out.String()
}

func TestSnapshotLeavesOutBytesTheNextFrameCarries(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	current, sink := newTestCapture(t, rec)

	// No newline: the bytes wait for the flush timer and no frame exists yet.
	if _, err := sink.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if state := current.snapshot(); state.OutputSeq != 0 || state.Output != "" {
		t.Fatalf("snapshot before the first frame: seq=%d output=%q", state.OutputSeq, state.Output)
	}
	if _, err := sink.Write([]byte(" done\nnext")); err != nil {
		t.Fatal(err)
	}
	state := current.snapshot()
	if state.OutputSeq != 1 || state.Output != "partial done\nnext" {
		t.Fatalf("snapshot after the first frame: seq=%d output=%q", state.OutputSeq, state.Output)
	}
	if frames := framesThrough(rec, state.OutputSeq); frames != state.Output {
		t.Fatalf("snapshot %q disagrees with its frames %q", state.Output, frames)
	}
}

func TestSnapshotMatchesItsFramesWhileOutputArrives(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	current, sink := newTestCapture(t, rec)

	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := range 400 {
			chunk := "word "
			if i%7 == 0 {
				chunk = "line\n"
			}
			if _, err := sink.Write([]byte(chunk)); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()
	for range 200 {
		state := current.snapshot()
		if frames := framesThrough(rec, state.OutputSeq); frames != state.Output {
			t.Fatalf("snapshot at seq %d holds %q, its frames hold %q", state.OutputSeq, state.Output, frames)
		}
	}
	writer.Wait()
}

package highlightapp

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/highlight"
)

// liveRecorder collects pushes and replays them as a client applies them: a
// push the fence has passed is ignored, a push From 0 replaces the fence, any
// other push must follow the last one. onEvent sees each applied push with the
// fence as it left it.
type liveRecorder struct {
	mu      sync.Mutex
	events  []LiveCodeEvent
	fences  map[string]*liveReplay
	gaps    []string
	onEvent func(LiveCodeEvent, liveReplay)
}

type liveReplay struct {
	seq    int
	lines  []highlight.EncodedLine
	hashes []uint32
	final  bool
	key    string
}

func newLiveRecorder() *liveRecorder {
	return &liveRecorder{fences: map[string]*liveReplay{}}
}

func (r *liveRecorder) emit(event LiveCodeEvent) {
	r.mu.Lock()
	r.events = append(r.events, event)
	key := fmt.Sprintf("%s|%s|%d", event.ThreadID, event.ItemID, event.Fence)
	entry := r.fences[key]
	switch {
	case entry != nil && event.Seq <= entry.seq:
		r.mu.Unlock()
		return
	case event.From == 0 && (event.LineHashes != nil || !event.Final):
		entry = &liveReplay{}
		r.fences[key] = entry
	case entry == nil || event.Seq != entry.seq+1:
		r.gaps = append(r.gaps, fmt.Sprintf("%s seq %d", key, event.Seq))
		r.mu.Unlock()
		return
	}
	entry.seq = event.Seq
	entry.lines = append(entry.lines[:min(event.From, len(entry.lines))], event.Lines...)
	entry.hashes = append(entry.hashes[:min(event.From, len(entry.hashes))], event.LineHashes...)
	entry.final, entry.key = event.Final, event.ContentKey
	if on := r.onEvent; on != nil {
		on(event, *entry)
	}
	r.mu.Unlock()
}

func (r *liveRecorder) snapshot() []LiveCodeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LiveCodeEvent(nil), r.events...)
}

func (r *liveRecorder) fence(thread, item string, index int) liveReplay {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.fences[fmt.Sprintf("%s|%s|%d", thread, item, index)]; entry != nil {
		return *entry
	}
	return liveReplay{}
}

func newLiveService(t *testing.T) (*Service, *liveRecorder) {
	t.Helper()
	rec := newLiveRecorder()
	service := New(Config{EmitLiveCode: rec.emit})
	t.Cleanup(service.Close)
	return service, rec
}

// waitPush waits for the push of the row's fence with seq and returns it.
func waitPush(t *testing.T, rec *liveRecorder, item string, fence, seq int) LiveCodeEvent {
	t.Helper()
	var found LiveCodeEvent
	waitFor(t, func() bool {
		for _, event := range rec.snapshot() {
			if event.ItemID == item && event.Fence == fence && event.Seq == seq {
				found = event
				return true
			}
		}
		return false
	})
	return found
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func finalFor(events []LiveCodeEvent, index int) (LiveCodeEvent, bool) {
	for _, event := range events {
		if event.Fence == index && event.Final {
			return event, true
		}
	}
	return LiveCodeEvent{}, false
}

const liveSampleGo = "func main() {\n\tx := `raw\nstring`\n\t/* note */\n\tfmt.Println(x, 42)\n}\n"

// TestLiveStreamsAFenceAsTheStatelessPathsHighlightIt streams rows in small
// deltas, checking every push against a whole highlight of the fence as
// streamed so far (the scan's announcement, seq 1, has no spans yet), and the
// final push against the stateless result the RPC and persistence paths
// compute. The plain fence's lines never change spans, so only their text
// tells the client a line grew.
func TestLiveStreamsAFenceAsTheStatelessPathsHighlightIt(t *testing.T) {
	for _, tc := range []struct{ lang, body string }{
		{"go", liveSampleGo},
		{"text", "plain words\ngrow one\nby one\n"},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			testLiveStream(t, tc.lang, tc.body)
		})
	}
}

func testLiveStream(t *testing.T, langName, body string) {
	service, rec := newLiveService(t)
	lang := highlight.LangFromName(langName)
	opener := "```" + langName + "\n"
	var checkMu sync.Mutex
	var failures []string
	streamed := ""
	rec.onEvent = func(event LiveCodeEvent, entry liveReplay) {
		if event.Final {
			return
		}
		checkMu.Lock()
		defer checkMu.Unlock()
		content := streamed[strings.Index(streamed, opener)+len(opener):]
		// The push describes the fence as of some delta; find it.
		for end := len(content); end >= 0; end-- {
			prefix := content[:end]
			if reflect.DeepEqual(entry.hashes, highlight.FrontendLineHashes(prefix)) {
				want := highlight.Highlight(lang, []byte(prefix)).Lines
				if want == nil || event.Seq == 1 {
					want = make([]highlight.EncodedLine, len(entry.hashes))
				}
				if !reflect.DeepEqual(entry.lines, want) {
					failures = append(failures, fmt.Sprintf("push %d: lines %v, want %v", event.Seq, entry.lines, want))
				}
				if head, _, _ := strings.Cut(prefix, "\n"); event.From == 0 && event.Head != head {
					failures = append(failures, fmt.Sprintf("push %d: head %q, want %q", event.Seq, event.Head, head))
				}
				return
			}
		}
		failures = append(failures, fmt.Sprintf("push %d: hashes match no streamed prefix", event.Seq))
	}
	text := "Intro text.\n" + opener + body + "```\nAfter."
	for i := 0; i < len(text); i += 3 {
		end := min(len(text), i+3)
		checkMu.Lock()
		streamed = text[:end]
		checkMu.Unlock()
		service.ObserveAssistantTextDelta("t", "i", "", text[i:end])
		time.Sleep(5 * time.Millisecond)
	}
	service.EndAssistantText("t", "i", text)
	waitFor(t, func() bool { return service.LiveItemCount() == 0 })

	checkMu.Lock()
	defer checkMu.Unlock()
	for _, failure := range failures {
		t.Error(failure)
	}
	source := strings.TrimSuffix(body, "\n")
	final, ok := finalFor(rec.snapshot(), 0)
	if !ok {
		t.Fatal("no final push")
	}
	if head, _, _ := strings.Cut(source, "\n"); final.ContentKey != highlight.FrontendContentKey(source) || final.From != 0 || final.Head != head {
		t.Fatalf("final push key %q from %d head %q", final.ContentKey, final.From, final.Head)
	}
	if want := highlight.Highlight(lang, []byte(source)).Lines; !reflect.DeepEqual(final.Lines, want) {
		t.Fatalf("final lines %v, want %v", final.Lines, want)
	}
	if !reflect.DeepEqual(final.LineHashes, highlight.FrontendLineHashes(source)) {
		t.Fatal("final hashes are not the source's chain")
	}
	if len(rec.gaps) > 0 {
		t.Fatalf("pushes a client could not apply in order: %v", rec.gaps)
	}
	if n := len(rec.snapshot()); n < 3 {
		t.Fatalf("%d pushes: the fence was not pushed while it streamed", n)
	}
	if got := service.live.treeBytes.Load(); got != 0 {
		t.Fatalf("tree budget holds %d bytes after the row ended", got)
	}
}

// TestLivePushesArePaced floods a row with deltas and checks one row pushes
// no more often than livePushInterval.
func TestLivePushesArePaced(t *testing.T) {
	service, rec := newLiveService(t)
	service.ObserveAssistantTextDelta("t", "i", "", "```go\n")
	start := time.Now()
	for time.Since(start) < 300*time.Millisecond {
		service.ObserveAssistantTextDelta("t", "i", "", "x := 1\n")
		time.Sleep(200 * time.Microsecond)
	}
	elapsed := time.Since(start)
	service.EndAssistantText("t", "i", "")
	waitFor(t, func() bool { return service.LiveItemCount() == 0 })
	pushes := 0
	for _, event := range rec.snapshot() {
		if !event.Final && event.Seq > 1 {
			pushes++
		}
	}
	if limit := int(elapsed/livePushInterval) + 2; pushes > limit {
		t.Fatalf("%d pushes in %v, want at most %d", pushes, elapsed, limit)
	}
}

func TestLiveResyncPushesTheOpenFenceWhole(t *testing.T) {
	service, rec := newLiveService(t)
	service.ObserveAssistantTextDelta("t", "i", "", "```go\nx := 1\n")
	waitPush(t, rec, "i", 0, 2)
	service.ObserveAssistantTextDelta("t", "i", "", "y := 2\n")
	if got := waitPush(t, rec, "i", 0, 3); got.From == 0 || got.Head != "" {
		t.Fatalf("an ordinary push resent the whole fence: %+v", got)
	}
	if open := service.ResyncLiveCode("t", "i"); open != 0 {
		t.Fatalf("resync reported open fence %d, want 0", open)
	}
	whole := waitPush(t, rec, "i", 0, 4)
	if whole.From != 0 || len(whole.LineHashes) != 3 || whole.Head != "x := 1" {
		t.Fatalf("resync push %+v, want the whole fence", whole)
	}
	if open := service.ResyncLiveCode("t", "missing"); open != -1 {
		t.Fatalf("resync of an unknown row reported fence %d", open)
	}
	service.ObserveAssistantTextDelta("t", "i", "", "```\nprose\n")
	waitFor(t, func() bool { _, ok := finalFor(rec.snapshot(), 0); return ok })
	if open := service.ResyncLiveCode("t", "i"); open != -1 {
		t.Fatalf("resync between fences reported fence %d, want -1", open)
	}
}

func TestLiveEndTexts(t *testing.T) {
	cases := []struct {
		name     string
		streamed string
		ends     []string
		want     map[int]string
	}{
		{"empty end finishes the streamed text", "```go\nx := 1\n", []string{""}, map[int]string{0: "x := 1\n"}},
		{"final text extends the stream", "```go\nx := 1\n", []string{"```go\nx := 1\ny := 2\n```\n"}, map[int]string{0: "x := 1\ny := 2"}},
		{"final text replaces the stream", "```go\nx := 1\n", []string{"```py\nz = 3\n```\n```go\nw := 4\n```"}, map[int]string{0: "z = 3", 1: "w := 4"}},
		{"an empty end after a stop keeps the stop's text", "```go\nx := 1\n", []string{"```go\nx := 1\ny := 2", ""}, map[int]string{0: "x := 1\ny := 2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, rec := newLiveService(t)
			// Hold the row's goroutine so every end arrives before it runs.
			service.live.slots <- struct{}{}
			service.ObserveAssistantTextDelta("t", "i", "", tc.streamed)
			for _, text := range tc.ends {
				service.EndAssistantText("t", "i", text)
			}
			<-service.live.slots
			waitFor(t, func() bool { return service.LiveItemCount() == 0 })
			events := rec.snapshot()
			for index, source := range tc.want {
				final, ok := finalFor(events, index)
				if !ok || final.ContentKey != highlight.FrontendContentKey(source) {
					t.Fatalf("fence %d final %+v, want key of %q", index, final, source)
				}
			}
		})
	}
}

func TestLiveFollowsOnlyFencesItCanHighlight(t *testing.T) {
	service, rec := newLiveService(t)
	service.ObserveAssistantTextDelta("t", "i", "", "```\nplain fence\n```\n```go\nx := \xff\n")
	service.EndAssistantText("t", "i", "")
	waitFor(t, func() bool { return service.LiveItemCount() == 0 })
	events := rec.snapshot()
	if len(events) != 2 || events[0].Fence != 1 || events[0].Seq != 1 {
		t.Fatalf("events %+v, want the invalid fence's announcement and stop", events)
	}
	if stop := events[1]; stop.Fence != 1 || stop.Seq != 2 || !stop.Final || stop.ContentKey != "" || stop.LineHashes != nil {
		t.Fatalf("stop push %+v", stop)
	}
}

func TestLiveTreeBudgetFallsBackToWholeParses(t *testing.T) {
	service, rec := newLiveService(t)
	big := strings.Repeat("x := 1\n", liveTreeBudget/7+10)
	service.ObserveAssistantTextDelta("t", "a", "", "```go\n"+big)
	waitPush(t, rec, "a", 0, 2)
	service.ObserveAssistantTextDelta("t", "b", "", "```go\ny := 2\n")
	waitPush(t, rec, "b", 0, 2)
	if got := service.live.treeBytes.Load(); got > liveTreeBudget {
		t.Fatalf("trees hold %d bytes, over the %d budget", got, liveTreeBudget)
	}
	service.EndAssistantText("t", "a", "")
	service.EndAssistantText("t", "b", "")
	waitFor(t, func() bool { return service.LiveItemCount() == 0 })
	if got := service.live.treeBytes.Load(); got != 0 {
		t.Fatalf("tree budget holds %d bytes after both rows ended", got)
	}
}

func TestLivePurgeAndCloseRelease(t *testing.T) {
	service, rec := newLiveService(t)
	service.ObserveAssistantTextDelta("t1", "i", "", "```go\nx := 1\n")
	service.ObserveAssistantTextDelta("t2", "i", "", "```go\nx := 1\n")
	waitFor(t, func() bool { return len(rec.snapshot()) == 4 })
	service.PurgeThread("t1")
	if got := service.LiveItemCount(); got != 1 {
		t.Fatalf("rows after purge = %d, want 1", got)
	}
	// The purged row's open fence is stopped, so a client showing it does
	// not wait for pushes that will not come.
	waitFor(t, func() bool { return len(rec.snapshot()) == 5 })
	stop := rec.snapshot()[4]
	if stop.ThreadID != "t1" || !stop.Final || stop.ContentKey != "" || stop.LineHashes != nil || stop.Seq != 3 {
		t.Fatalf("purge push = %+v, want a stop of the open fence", stop)
	}
	service.ObserveAssistantTextDelta("t1", "i", "", "")
	service.Close()
	if got := service.live.treeBytes.Load(); got != 0 {
		t.Fatalf("tree budget holds %d bytes after Close", got)
	}
	before := len(rec.snapshot())
	service.ObserveAssistantTextDelta("t2", "i", "", "y := 2\n")
	service.EndAssistantText("t2", "i", "")
	time.Sleep(50 * time.Millisecond)
	if after := len(rec.snapshot()); after != before {
		t.Fatalf("pushes after Close: %d, want %d", after, before)
	}
}

// TestLiveAnnouncesAFenceBeforeTheCallerEmitsItsText: the delta that gives a
// followed fence its first text pushes seq 1 before Observe returns, with the
// text's line hashes and first line and no spans. The row's goroutine is held,
// so only the scan can push.
func TestLiveAnnouncesAFenceBeforeTheCallerEmitsItsText(t *testing.T) {
	service, rec := newLiveService(t)
	service.live.slots <- struct{}{}
	service.ObserveAssistantTextDelta("t", "i", "", "Intro\n```go\n")
	if events := rec.snapshot(); len(events) != 0 {
		t.Fatalf("pushes for a fence with no text: %+v", events)
	}
	service.ObserveAssistantTextDelta("t", "i", "", "x := 1\ny")
	events := rec.snapshot()
	want := LiveCodeEvent{
		ThreadID: "t", ItemID: "i", Fence: 0, Lang: "go", Seq: 1, From: 0,
		LineHashes: highlight.FrontendLineHashes("x := 1\ny"), Lines: make([]highlight.EncodedLine, 2), Head: "x := 1",
	}
	if len(events) != 1 || !reflect.DeepEqual(events[0], want) {
		t.Fatalf("announcement %+v, want %+v", events, want)
	}
	// Later text, a plain fence and a followed one that opens and closes in
	// one delta: only the followed one is announced.
	service.ObserveAssistantTextDelta("t", "i", "", " := 2\n```\n```\nplain\n```\n```py\na = 1\n```\n")
	events = rec.snapshot()
	if len(events) != 2 || events[1].Fence != 2 || events[1].Seq != 1 || events[1].Head != "a = 1" {
		t.Fatalf("pushes %+v, want one announcement of fence 2", events)
	}
	<-service.live.slots
	service.EndAssistantText("t", "i", "")
	waitFor(t, func() bool { return service.LiveItemCount() == 0 })
	if len(rec.gaps) > 0 {
		t.Fatalf("pushes a client could not apply in order: %v", rec.gaps)
	}
	for _, fence := range []int{0, 2} {
		if got := rec.fence("t", "i", fence); !got.final || got.key == "" {
			t.Fatalf("fence %d ended %+v, want its final spans", fence, got)
		}
	}
}

// TestLiveResyncNamesTheFenceTheScanOpened: Resync reports the fence the scan
// last opened even before the row's goroutine reaches it, and the goroutine
// finishing the fence before it does not clear it.
func TestLiveResyncNamesTheFenceTheScanOpened(t *testing.T) {
	service, rec := newLiveService(t)
	service.live.slots <- struct{}{}
	service.ObserveAssistantTextDelta("t", "i", "", "```go\nx := 1\n```\n```py\na = 1\n")
	if open := service.ResyncLiveCode("t", "i"); open != 1 {
		t.Fatalf("resync reported fence %d, want 1", open)
	}
	<-service.live.slots
	if final := waitPush(t, rec, "i", 0, 2); !final.Final {
		t.Fatalf("fence 0 push %+v, want its final", final)
	}
	if whole := waitPush(t, rec, "i", 1, 2); whole.From != 0 || whole.Head != "a = 1" {
		t.Fatalf("fence 1 push %+v, want the whole fence", whole)
	}
	if open := service.ResyncLiveCode("t", "i"); open != 1 {
		t.Fatalf("resync after fence 0 finished reported %d, want 1", open)
	}
}

// TestLivePurgeEndsAFenceOnlyTheScanReached: a row purged before its goroutine
// reached an announced fence still ends it.
func TestLivePurgeEndsAFenceOnlyTheScanReached(t *testing.T) {
	service, rec := newLiveService(t)
	service.live.slots <- struct{}{}
	service.ObserveAssistantTextDelta("t", "i", "", "```go\nx := 1\n")
	service.PurgeThread("t")
	<-service.live.slots
	stop := waitPush(t, rec, "i", 0, 2)
	if !stop.Final || stop.ContentKey != "" || stop.LineHashes != nil {
		t.Fatalf("purge push %+v, want a stop", stop)
	}
}

// TestLiveReplacedTextFinalsOutrankStreamedPushes: a final text that replaces
// the streamed one pushes finals a client applies over the streamed pushes.
func TestLiveReplacedTextFinalsOutrankStreamedPushes(t *testing.T) {
	service, rec := newLiveService(t)
	service.ObserveAssistantTextDelta("t", "i", "", "```go\nx := 1\n")
	waitPush(t, rec, "i", 0, 2)
	service.ObserveAssistantTextDelta("t", "i", "", "y := 2\n")
	waitPush(t, rec, "i", 0, 3)
	service.EndAssistantText("t", "i", "```go\nz := 3\n```")
	waitFor(t, func() bool { return service.LiveItemCount() == 0 })
	if got := rec.fence("t", "i", 0); !got.final || got.key != highlight.FrontendContentKey("z := 3") {
		t.Fatalf("fence 0 ended %+v, want the replacement's final", got)
	}
}

func TestHeadOfLeavesOutACutOffCharacter(t *testing.T) {
	for source, want := range map[string]string{
		"ab\ncd": "ab", "ab\xc3": "ab", "a\u00e9": "a\u00e9", "": "", "\u00e9\xe2\x82": "\u00e9",
	} {
		if got := headOf([]byte(source)); got != want {
			t.Errorf("headOf(%q) = %q, want %q", source, got, want)
		}
	}
}

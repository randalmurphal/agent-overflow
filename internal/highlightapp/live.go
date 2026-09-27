package highlightapp

import (
	"bytes"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/highlight"
)

// Live highlighting of the code fences in streaming assistant text. Triage
// hands the service each text delta before it emits it. The service scans the
// delta for fences at once; one goroutine per streaming row feeds the open
// fence's highlight.Stream and pushes the lines whose spans or text changed,
// at most once per livePushInterval. Every client watching the thread gets
// the same pushes, so no client asks the backend to re-highlight text the
// backend already has.
//
// A fence's first push, seq 1, is made during the scan of the delta that
// gives the fence its first text: its lines' hashes and first line, without
// spans. The caller emits the delta after it, so a client has every fence the
// service follows before it has any of the fence's text, and never requests
// spans for text a push will describe.
//
// A push carries no text: LineHashes is the frontend's cumulative per-line
// hash chain (highlight.FrontendLineHashes) for the lines it covers, so a
// client proves which of its own lines the spans describe. Seq numbers a
// fence's pushes; a push with From 0 carries the whole fence and applies on
// its own and carries the fence's first line as Head. A client that misses a
// push asks for one through Resync.
//
// A fence's final push carries the whole fence as the stateless highlight of
// its final source, the result every other path computes for that text, and
// ContentKey names it for the client's span cache. A final push without
// ContentKey ends a fence the service stopped following (over a cap, invalid
// UTF-8, a failed parse), whose client falls back to requesting spans.

const (
	// livePushInterval is the least time between one row's pushes. The
	// frontend reveals text at a steady pace behind the wire, so pushes at
	// this rate color text before it is revealed.
	livePushInterval = 33 * time.Millisecond
	// liveCostFactor spaces a row's pushes by this multiple of what its last
	// push cost, so a fence that must reparse whole on every push (grammars
	// with injections, fences past the tree budget) spends at most a quarter
	// of a core.
	liveCostFactor = 4
	// liveTreeBudget bounds the fence source whose syntax trees stay held
	// between pushes. A tree costs about 46 bytes per source byte; fences past
	// the budget reparse whole on each push instead.
	liveTreeBudget = 1 << 20
)

// LiveCodeEvent is one push for one fence of a streaming row.
type LiveCodeEvent struct {
	ThreadID string
	ItemID   string
	// ParentID is the row's transcript scope, "" for a root row.
	ParentID string
	// Fence is the fence's ordinal among the row's fences.
	Fence int
	// Lang is the fence's info word, as the frontend's markdown lexer reports
	// it.
	Lang string
	// Seq numbers the fence's pushes from 1.
	Seq int
	// From is the first line this push replaces. Lines and LineHashes cover
	// lines From to the end of the fence.
	From       int
	LineHashes []uint32
	Lines      []highlight.EncodedLine
	Final      bool
	// ContentKey is the frontend contentKey of the fence's final source, on
	// a final push that describes it.
	ContentKey string
	// Head is the fence's first line, on a push From 0 that has lines. Until
	// the client's first line or the fence's is complete, no line hash can
	// prove the fence is the client's block; a prefix relation to Head does.
	Head string
}

type liveHighlighter struct {
	mu        sync.Mutex
	items     map[string]*liveItem
	closed    bool
	stop      chan struct{}
	wg        sync.WaitGroup
	slots     chan struct{}
	treeBytes atomic.Int64
}

// liveItem is one streaming row. The fields under the service lock are what
// callers hand over; the rest belongs to the row's goroutine.
type liveItem struct {
	key, threadID, itemID, parentID string
	wake                            chan struct{}

	// Guarded by liveHighlighter.mu.
	pending []byte
	// scan is fed on the caller's goroutine; steps holds its steps for the
	// row's goroutine, and spare is the batch that goroutine last took. A
	// step's Text may alias pending.
	scan    highlight.FenceStream
	steps   []highlight.FenceStep
	spare   []highlight.FenceStep
	ended   bool
	endText string
	resync  bool
	purged  bool
	// open is the index of the last followed fence the scan opened while it
	// is open, or -1.
	open int
	// unannounced is the index of a followed fence that has had no text yet,
	// or -1, and unannouncedLang its language.
	unannounced     int
	unannouncedLang string

	fence    *liveFence
	streamed int
	digest   uint64
	next     time.Time
	// maxSeq is the highest Seq the row's goroutine pushed for any fence.
	maxSeq int
}

// liveFence is the row's open fence.
type liveFence struct {
	index int
	lang  string
	// stream is nil for a fence the service does not follow: one without a
	// language, or one it stopped following.
	stream *highlight.Stream
	chain  highlight.LineHashChain
	seq    int
	// sent is the fence's line count at its last push.
	sent int
	// changed is the first line whose spans changed since the last push.
	changed int
	dirty   bool
	full    bool
	// tree is the source length this fence holds against the tree budget.
	tree int
	// valid is how far the source has been checked as UTF-8.
	valid int
}

func (l *liveHighlighter) init() {
	l.items = map[string]*liveItem{}
	l.stop = make(chan struct{})
	l.slots = make(chan struct{}, liveWorkerSlots())
}

// ObserveAssistantTextDelta hands the service the next text a streaming
// assistant row is about to emit, with the row's transcript scope. It pushes
// the first push of each fence the delta gives its first text, then returns;
// it never blocks on highlighting.
func (s *Service) ObserveAssistantTextDelta(threadID, itemID, parentID, delta string) {
	if delta == "" {
		return
	}
	l := &s.live
	key := threadID + "|" + itemID
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	item := l.items[key]
	if item == nil {
		item = &liveItem{key: key, threadID: threadID, itemID: itemID, parentID: parentID, wake: make(chan struct{}, 1), digest: liveDigestSeed, open: -1, unannounced: -1}
		l.items[key] = item
		l.wg.Add(1)
		go s.runLiveItem(item)
	}
	start, first := len(item.pending), len(item.steps)
	item.pending = append(item.pending, delta...)
	item.steps = item.scan.Write(item.pending[start:], item.steps)
	announcements := item.announcements(item.steps[first:])
	l.mu.Unlock()
	for _, event := range announcements {
		s.config.EmitLiveCode(event)
	}
	item.signal()
}

// announcements records the fences steps open and returns the first push of
// each followed fence they give its first text. Called under the service lock.
func (item *liveItem) announcements(steps []highlight.FenceStep) []LiveCodeEvent {
	var events []LiveCodeEvent
	var text []byte
	announce := func() {
		if text == nil {
			return
		}
		source := string(text)
		hashes := highlight.FrontendLineHashes(source)
		events = append(events, LiveCodeEvent{
			ThreadID: item.threadID, ItemID: item.itemID, ParentID: item.parentID, Fence: item.unannounced,
			Lang: item.unannouncedLang, Seq: 1, LineHashes: hashes, Lines: make([]highlight.EncodedLine, len(hashes)),
			Head: headOf(text),
		})
		item.unannounced, text = -1, nil
	}
	for _, step := range steps {
		switch step.Kind {
		case highlight.FenceOpened:
			if step.Lang != "" {
				item.open, item.unannounced, item.unannouncedLang = step.Index, step.Index, step.Lang
			}
		case highlight.FenceContent:
			if step.Index == item.unannounced {
				text = append(text, step.Text...)
			}
		case highlight.FenceClosed:
			announce()
			if step.Index == item.unannounced {
				item.unannounced = -1
			}
		}
	}
	announce()
	return events
}

// headOf is the first line of a fence's source. A character cut off at the
// end is left out until the rest of it arrives, as the line hashes leave it.
func headOf(source []byte) string {
	if end := bytes.IndexByte(source, '\n'); end >= 0 {
		return string(source[:end])
	}
	end := len(source)
	for cut := end - 1; cut >= 0 && cut >= end-utf8.UTFMax; cut-- {
		if utf8.RuneStart(source[cut]) {
			if !utf8.FullRune(source[cut:]) {
				end = cut
			}
			break
		}
	}
	return string(source[:end])
}

// EndAssistantText ends a streaming row. text is the row's final model text,
// or empty when the row ended without one; the service finishes the fences
// of whichever text is final and releases the row.
func (s *Service) EndAssistantText(threadID, itemID, text string) {
	l := &s.live
	l.mu.Lock()
	item := l.items[threadID+"|"+itemID]
	if item == nil {
		l.mu.Unlock()
		return
	}
	// A stop ends a row with its text and the settle after it ends the row
	// again without; the text stays.
	if text != "" || !item.ended {
		item.endText = text
	}
	item.ended = true
	l.mu.Unlock()
	item.signal()
}

// ResyncLiveCode makes the row's next push carry its open fence whole, for a
// client that missed a push. It returns the open fence the service follows,
// or -1 when there is none: every other fence of the row has had its final
// push.
func (s *Service) ResyncLiveCode(threadID, itemID string) int {
	l := &s.live
	l.mu.Lock()
	item := l.items[threadID+"|"+itemID]
	if item == nil || item.ended || item.open < 0 {
		l.mu.Unlock()
		return -1
	}
	open := item.open
	item.resync = true
	l.mu.Unlock()
	item.signal()
	return open
}

// PurgeThread drops the thread's streaming rows. Each row's open fence gets a
// stop push, so a client showing it requests its spans instead of waiting for
// pushes that will not come.
func (s *Service) PurgeThread(threadID string) {
	l := &s.live
	prefix := threadID + "|"
	l.mu.Lock()
	var purged []*liveItem
	for key, item := range l.items {
		if strings.HasPrefix(key, prefix) {
			item.purged = true
			delete(l.items, key)
			purged = append(purged, item)
		}
	}
	l.mu.Unlock()
	for _, item := range purged {
		item.signal()
	}
}

// LiveItemCount reports the streaming rows the service follows.
func (s *Service) LiveItemCount() int {
	s.live.mu.Lock()
	defer s.live.mu.Unlock()
	return len(s.live.items)
}

// Close stops live highlighting and waits for every row's goroutine to
// release its trees. Later deltas are ignored.
func (s *Service) Close() {
	l := &s.live
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.stop)
	}
	l.mu.Unlock()
	l.wg.Wait()
}

func (item *liveItem) signal() {
	select {
	case item.wake <- struct{}{}:
	default:
	}
}

// runLiveItem is the row's goroutine: it waits for input, paces pushes, and
// releases the row when it ends, is purged, or the service closes.
func (s *Service) runLiveItem(item *liveItem) {
	l := &s.live
	defer l.wg.Done()
	defer s.releaseLiveFence(item)
	pace := time.NewTimer(0)
	defer pace.Stop()
	<-pace.C
	for {
		select {
		case <-item.wake:
		case <-l.stop:
			return
		}
		if wait := time.Until(item.next); wait > 0 {
			pace.Reset(wait)
			select {
			case <-pace.C:
			case <-l.stop:
				return
			}
		}
		select {
		case l.slots <- struct{}{}:
		case <-l.stop:
			return
		}
		l.mu.Lock()
		pending, steps, ended, endText, resync, purged := item.pending, item.steps, item.ended, item.endText, item.resync, item.purged
		item.pending, item.steps, item.spare, item.resync = nil, item.spare[:0], steps, false
		l.mu.Unlock()
		if purged {
			// The scan announced the fences in steps; each gets its end.
			s.applyFenceSteps(item, steps)
			if item.fence != nil && item.fence.stream != nil {
				s.stopLiveFence(item, item.fence)
			}
			<-l.slots
			return
		}
		started := time.Now()
		s.advanceLiveItem(item, pending, steps, resync)
		clear(steps)
		if ended {
			s.endLiveItem(item, endText)
		}
		<-l.slots
		if ended {
			l.mu.Lock()
			if l.items[item.key] == item {
				delete(l.items, item.key)
			}
			l.mu.Unlock()
			return
		}
		item.next = time.Now().Add(max(livePushInterval, liveCostFactor*time.Since(started)))
	}
}

// advanceLiveItem feeds the scanned steps to the row's fences and pushes the
// open fence's changes.
func (s *Service) advanceLiveItem(item *liveItem, pending []byte, steps []highlight.FenceStep, resync bool) {
	item.streamed += len(pending)
	item.digest = foldDigest(item.digest, pending)
	s.applyFenceSteps(item, steps)
	fence := item.fence
	if fence == nil || fence.stream == nil {
		return
	}
	if resync {
		fence.full, fence.dirty = true, true
	}
	if fence.dirty {
		s.pushLiveFence(item, fence)
	}
}

// endLiveItem finishes the row with its final text: the streamed text, an
// extension of it, or a replacement whose fences are pushed final afresh.
func (s *Service) endLiveItem(item *liveItem, text string) {
	switch {
	case text == "" || (len(text) == item.streamed && foldDigest(liveDigestSeed, []byte(text)) == item.digest):
	case len(text) > item.streamed && foldDigest(liveDigestSeed, []byte(text[:item.streamed])) == item.digest:
		s.live.mu.Lock()
		steps := item.scan.Write([]byte(text[item.streamed:]), nil)
		s.live.mu.Unlock()
		s.applyFenceSteps(item, steps)
	default:
		// Each fence's final push outranks any push of the streamed text
		// that shares its index.
		s.releaseLiveFence(item)
		item.fence = nil
		seq := max(item.maxSeq, 1) + 1
		for index, fence := range highlight.ScanFences(text) {
			if fence.Lang != "" {
				s.pushFinalFence(item, index, fence.Lang, seq, fence.Source)
			}
		}
		return
	}
	s.live.mu.Lock()
	steps := item.scan.Finish(nil)
	s.live.mu.Unlock()
	s.applyFenceSteps(item, steps)
	if fence := item.fence; fence != nil {
		s.finishLiveFence(item, fence, false)
		item.fence = nil
	}
}

// applyFenceSteps applies scanned steps to the row's fences. A tick's content
// for one fence is appended in one piece: each append reparses.
func (s *Service) applyFenceSteps(item *liveItem, steps []highlight.FenceStep) {
	var content []byte
	flush := func() {
		if fence := item.fence; fence != nil && fence.stream != nil && len(content) > 0 {
			s.appendLiveFence(item, fence, content)
		}
		content = content[:0]
	}
	for _, step := range steps {
		switch step.Kind {
		case highlight.FenceOpened:
			item.fence = &liveFence{index: step.Index, lang: step.Lang}
			if step.Lang != "" {
				// Seq 1 is the scan's announcement.
				item.fence.stream = highlight.NewStream(highlight.LangFromName(step.Lang))
				item.fence.seq = 1
			}
		case highlight.FenceContent:
			content = append(content, step.Text...)
		case highlight.FenceClosed:
			flush()
			if fence := item.fence; fence != nil {
				s.finishLiveFence(item, fence, true)
				item.fence = nil
			}
		}
	}
	flush()
}

// appendLiveFence appends content to the fence's stream. A fence that goes
// over a cap or holds invalid UTF-8 ends with a final push that stops it.
func (s *Service) appendLiveFence(item *liveItem, fence *liveFence, text []byte) {
	if !s.validLiveText(fence, text) {
		s.stopLiveFence(item, fence)
		return
	}
	from, state := fence.stream.Append(text, s.reserveTree(fence, len(text)))
	if !fence.stream.HoldsTree() {
		s.releaseTree(fence)
	}
	switch state {
	case highlight.StreamOverCap:
		s.stopLiveFence(item, fence)
	case highlight.StreamOK:
		fence.changed = min(fence.changed, from)
		fence.dirty = true
	}
}

// validLiveText checks the fence's source as UTF-8 as it grows. A character
// cut off at the end of text is checked once the rest of it arrives.
func (s *Service) validLiveText(fence *liveFence, text []byte) bool {
	source := fence.stream.Source()
	unchecked := append(source[fence.valid:len(source):len(source)], text...)
	end := len(unchecked)
	for cut := end - 1; cut >= 0 && cut >= end-utf8.UTFMax; cut-- {
		if utf8.RuneStart(unchecked[cut]) {
			if !utf8.FullRune(unchecked[cut:]) {
				end = cut
			}
			break
		}
	}
	if !utf8.Valid(unchecked[:end]) {
		return false
	}
	fence.valid += end
	return true
}

func (s *Service) pushLiveFence(item *liveItem, fence *liveFence) {
	lines := fence.stream.LineCount()
	from := fence.changed
	if fence.sent > 0 {
		// The last line pushed may have grown since.
		from = min(from, fence.sent-1)
	}
	if fence.full || fence.sent == 0 {
		from = 0
	}
	from = min(from, lines-1)
	fence.seq++
	event := LiveCodeEvent{
		ThreadID: item.threadID, ItemID: item.itemID, ParentID: item.parentID, Fence: fence.index, Lang: fence.lang,
		Seq: fence.seq, From: from,
		LineHashes: fence.chain.Hashes(fence.stream.Source(), from),
		Lines:      fence.stream.Lines(from),
	}
	if from == 0 {
		event.Head = headOf(fence.stream.Source())
	}
	s.emitLive(item, event)
	fence.sent, fence.changed, fence.dirty, fence.full = lines, lines, false, false
}

// endLiveOpen records that the fence the scan last opened is no longer
// followed, unless the scan has opened another since.
func (s *Service) endLiveOpen(item *liveItem, index int) {
	s.live.mu.Lock()
	if item.open == index {
		item.open = -1
	}
	s.live.mu.Unlock()
}

func (s *Service) emitLive(item *liveItem, event LiveCodeEvent) {
	item.maxSeq = max(item.maxSeq, event.Seq)
	s.config.EmitLiveCode(event)
}

// finishLiveFence pushes the fence's final state and releases it. A fence
// the closing fence ended has the newline before the closer in its content;
// the final source leaves it out, as the markdown lexer does.
func (s *Service) finishLiveFence(item *liveItem, fence *liveFence, closed bool) {
	if fence.stream == nil {
		return
	}
	s.endLiveOpen(item, fence.index)
	source := string(fence.stream.Source())
	if closed {
		source = strings.TrimSuffix(source, "\n")
	}
	s.releaseTree(fence)
	fence.stream.Close()
	fence.stream = nil
	s.pushFinalFence(item, fence.index, fence.lang, fence.seq+1, source)
}

// pushFinalFence pushes a fence whole, highlighted as the stateless paths
// highlight its final source.
func (s *Service) pushFinalFence(item *liveItem, index int, lang string, seq int, source string) {
	event := LiveCodeEvent{ThreadID: item.threadID, ItemID: item.itemID, ParentID: item.parentID, Fence: index, Lang: lang, Seq: seq, Final: true}
	if utf8.ValidString(source) {
		res := s.cache.Code(highlight.LangFromName(lang), source)
		if !res.Incomplete {
			event.LineHashes = highlight.FrontendLineHashes(source)
			event.Lines = res.Lines
			event.ContentKey = highlight.FrontendContentKey(source)
			event.Head, _, _ = strings.Cut(source, "\n")
		}
	}
	s.emitLive(item, event)
}

// stopLiveFence ends a fence the service will not follow further: its final
// push carries no content, and the client requests spans for what follows.
func (s *Service) stopLiveFence(item *liveItem, fence *liveFence) {
	s.endLiveOpen(item, fence.index)
	s.releaseTree(fence)
	fence.stream.Close()
	fence.stream = nil
	fence.seq++
	s.emitLive(item, LiveCodeEvent{
		ThreadID: item.threadID, ItemID: item.itemID, ParentID: item.parentID, Fence: fence.index, Lang: fence.lang,
		Seq: fence.seq, From: fence.sent, Final: true,
	})
}

// releaseLiveFence frees the row's open fence without pushing.
func (s *Service) releaseLiveFence(item *liveItem) {
	if fence := item.fence; fence != nil && fence.stream != nil {
		s.releaseTree(fence)
		fence.stream.Close()
		fence.stream = nil
	}
}

// reserveTree reports whether the fence may hold its tree after growing by
// n bytes, charging the growth against the shared budget.
func (s *Service) reserveTree(fence *liveFence, n int) bool {
	want := len(fence.stream.Source()) + n - fence.tree
	if s.live.treeBytes.Add(int64(want)) > liveTreeBudget {
		s.live.treeBytes.Add(-int64(want))
		return false
	}
	fence.tree += want
	return true
}

func (s *Service) releaseTree(fence *liveFence) {
	if fence.tree > 0 {
		s.live.treeBytes.Add(-int64(fence.tree))
		fence.tree = 0
	}
}

// liveDigestSeed starts the FNV-1a digest of a row's streamed text, which
// tells a final text that extends the stream from one that replaces it.
const liveDigestSeed uint64 = 14695981039346656037

func foldDigest(digest uint64, p []byte) uint64 {
	for _, b := range p {
		digest = (digest ^ uint64(b)) * 1099511628211
	}
	return digest
}

func liveWorkerSlots() int {
	return max(1, runtime.GOMAXPROCS(0)/2)
}

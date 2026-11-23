package claudetui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/testutil/mockexec"
)

// exitRecorder records a session's events and closes disconnected when the
// "disconnected" status arrives. hold, when set, runs before an event is
// recorded, on the emitting goroutine.
type exitRecorder struct {
	eventRecorder
	hold         func(provider.ProviderEvent)
	once         sync.Once
	disconnected chan struct{}
}

func newExitRecorder() *exitRecorder {
	return &exitRecorder{disconnected: make(chan struct{})}
}

func (r *exitRecorder) record(e provider.ProviderEvent) {
	if r.hold != nil {
		r.hold(e)
	}
	r.eventRecorder.record(e)
	if isDisconnected(e) {
		r.once.Do(func() { close(r.disconnected) })
	}
}

func isDisconnected(e provider.ProviderEvent) bool {
	return e.Kind == provider.EventSessionStatus && e.Content == "disconnected"
}

func (r *exitRecorder) waitDisconnected(t *testing.T) {
	t.Helper()
	select {
	case <-r.disconnected:
	case <-time.After(10 * time.Second):
		t.Fatalf("no \"disconnected\" status; events %v", kindsOf(r.snapshot()))
	}
}

// gate blocks the emitting goroutine at the first event match accepts until
// open; reached closes when it is blocked there.
type gate struct {
	reached  chan struct{}
	opened   chan struct{}
	once     sync.Once
	openOnce sync.Once
}

func newGate() *gate {
	return &gate{reached: make(chan struct{}), opened: make(chan struct{})}
}

// newHeldSession is newTestSession with held opened at cleanup before the
// session's Close, which would otherwise wait on the held emission.
func newHeldSession(t *testing.T, ctx context.Context, cfg Config, rec *exitRecorder, held *gate) *Session {
	t.Helper()
	s := newTestSession(t, ctx, cfg, rec)
	t.Cleanup(held.open)
	return s
}

func (g *gate) holdAt(match func(provider.ProviderEvent) bool) func(provider.ProviderEvent) {
	return func(e provider.ProviderEvent) {
		if !match(e) {
			return
		}
		first := false
		g.once.Do(func() { first = true })
		if first {
			close(g.reached)
			<-g.opened
		}
	}
}

func (g *gate) open() { g.openOnce.Do(func() { close(g.opened) }) }

func (g *gate) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the held event was never emitted")
	}
}

// tuiExitConfig launches script in place of the CLI with both prompt files
// in promptDir.
func tuiExitConfig(t *testing.T, script, promptDir string) Config {
	t.Helper()
	return withTestRetention(Config{
		Reaper:             procutiltest.Reaper(),
		Binary:             mockexec.Write(t, t.TempDir()+"/claude", script),
		WorkDir:            t.TempDir(),
		HookCmd:            "/bin/true",
		SystemPromptDir:    promptDir,
		SystemPrompt:       "REPLACEMENT",
		AppendSystemPrompt: "APPENDED GUIDE",
	})
}

// readLineScript reads one line from its terminal, then ends as ending says.
func readLineScript(ending string) string {
	return "#!/bin/sh\nread line\n" + ending + "\n"
}

func newTestSession(t *testing.T, ctx context.Context, cfg Config, rec *exitRecorder) *Session {
	t.Helper()
	s, err := NewSession(ctx, testThread, cfg, rec.record)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func endProcess(t *testing.T, s *Session) {
	t.Helper()
	if err := s.writePTY([]byte("\n")); err != nil {
		t.Fatalf("write the line: %v", err)
	}
}

func dialable(addr string) bool {
	conn, err := net.Dial("tcp6", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func addrOf(rawURL string) string {
	return strings.TrimPrefix(rawURL, "http://")
}

// waitRefused waits until addr refuses connections: the server's stop has
// closed its listener. A listener's close has no other observable handle,
// so this polls, bounded.
func waitRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for dialable(addr) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still accepts connections", addr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func allStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitParked waits until want goroutines are blocked (their state contains
// state) in fn. A goroutine blocked inside a function offers no handle of
// its own, so this polls the stacks, bounded; failed, when it has a value,
// ends the wait early as a failure.
func waitParked(t *testing.T, state, fn string, want int, failed <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		parked := 0
		for _, g := range strings.Split(allStacks(), "\n\n") {
			header, rest, _ := strings.Cut(g, "\n")
			if strings.Contains(header, state) && strings.Contains(rest, fn) {
				parked++
			}
		}
		if parked >= want {
			return
		}
		select {
		case err := <-failed:
			t.Fatalf("returned (%v) instead of waiting in %s", err, fn)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines wait in %s, want %d\n%s", parked, fn, want, allStacks())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const closeFrame = "agent-overflow/internal/provider/claudetui.(*Session).closeFrom("

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func eventLines(events []provider.ProviderEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s:%s", e.Kind, e.Content))
	}
	return out
}

func statusEvents(events []provider.ProviderEvent) []string {
	var out []string
	for _, e := range events {
		if e.Kind == provider.EventSessionStatus {
			out = append(out, e.Content)
		}
	}
	return out
}

func assertLastIsDisconnected(t *testing.T, events []provider.ProviderEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if last := events[len(events)-1]; !isDisconnected(last) {
		t.Fatalf("last event = %v %q, want \"disconnected\"; events %q", last.Kind, last.Content, eventLines(events))
	}
}

// parserHoldsState reports whether the parser still remembers a tool_use,
// which Parser.Close forgets.
func parserHoldsState(p *claude.Parser) bool {
	return !reflect.ValueOf(p).Elem().FieldByName("toolUseNames").IsZero()
}

func toolUseEnvelope(id string) json.RawMessage {
	return json.RawMessage(`{"type":"assistant","message":{"id":"msg_` + id + `","type":"message","role":"assistant","model":"claude-x","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"ls"}}]}}`)
}

func toolStartIDs(events []provider.ProviderEvent) []string {
	var out []string
	for _, e := range events {
		if e.Kind == provider.EventToolStart {
			out = append(out, e.ItemID)
		}
	}
	return out
}

// assertReleased checks everything a live session holds is gone: both
// listeners refuse connections, their serve goroutines and the feed loop
// have returned, the session is complete, the parser forgot its state and
// both prompt files are removed.
func assertReleased(t *testing.T, s *Session, promptDir string) {
	t.Helper()
	for _, addr := range []string{addrOf(s.gateway.baseURL()), addrOf(s.relay.url())} {
		if dialable(addr) {
			t.Errorf("listener %s still accepts connections", addr)
		}
	}
	for name, ch := range map[string]chan struct{}{
		"the gateway's serve goroutine": s.gateway.srv.serveDone,
		"the relay's serve goroutine":   s.relay.srv.serveDone,
		"the feed loop":                 s.feedDone,
	} {
		if !closed(ch) {
			t.Errorf("%s has not returned", name)
		}
	}
	if !closed(s.Connection().Done()) {
		t.Error("the session is not complete")
	}
	if parserHoldsState(s.parser) {
		t.Error("parser state was not released")
	}
	if left := promptFilesIn(t, promptDir); len(left) > 0 {
		t.Errorf("prompt files left behind: %v", left)
	}
}

// A process that ends on its own, cleanly, with an error code, or by a
// signal, releases everything the session held for it with nobody calling
// Close, before its teardown pair, which keeps its events and meta. A
// later Close returns at once and emits nothing.
func TestASelfExitReleasesTheSession(t *testing.T) {
	for name, tc := range map[string]struct {
		ending string
		want   provider.ProcessExitInfo
	}{
		"exit 0":  {"exit 0", provider.ProcessExitInfo{Reason: "provider session ended"}},
		"exit 3":  {"exit 3", provider.ProcessExitInfo{Reason: "provider session exited with code 3", ExitCode: 3}},
		"kill -9": {"kill -9 $$", provider.ProcessExitInfo{Reason: "provider session terminated by signal killed", Signal: "killed"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := newExitRecorder()
			promptDir := t.TempDir()
			s := newTestSession(t, context.Background(), tuiExitConfig(t, readLineScript(tc.ending), promptDir), rec)
			if left := promptFilesIn(t, promptDir); len(left) != 2 {
				t.Fatalf("prompt files = %v, want both", left)
			}
			s.feedEnvelope(toolUseEnvelope("toolu_exit"))
			rec.waitForKind(t, provider.EventToolStart)
			if !parserHoldsState(s.parser) {
				t.Fatal("parser holds no state after a tool_use")
			}

			endProcess(t, s)
			rec.waitDisconnected(t)
			assertReleased(t, s, promptDir)

			events := rec.snapshot()
			assertLastIsDisconnected(t, events)
			if got := statusEvents(events); !slices.Equal(got, []string{"error", "disconnected"}) {
				t.Fatalf("statuses = %v, want [error disconnected]", got)
			}
			var info provider.ProcessExitInfo
			for _, e := range events {
				if e.Kind == provider.EventSessionStatus && e.Content == "error" {
					if err := json.Unmarshal(e.Meta, &info); err != nil {
						t.Fatalf("decode exit meta %s: %v", e.Meta, err)
					}
				}
			}
			if info != tc.want {
				t.Errorf("exit meta = %+v, want %+v", info, tc.want)
			}

			if err := s.Close(); err != nil {
				t.Fatalf("Close after a self-exit: %v", err)
			}
			if after := rec.snapshot(); len(after) != len(events) {
				t.Fatalf("Close emitted %v after the release", kindsOf(after[len(events):]))
			}
		})
	}
}

// Envelopes the feed accepted before the process ended are all parsed and
// emitted, in order, before the teardown pair: here the parser goroutine is
// held on an earlier event while 20 more wait in the feed and the process
// exits.
func TestASelfExitParsesTheFeedBacklog(t *testing.T) {
	rec := newExitRecorder()
	held := newGate()
	rec.hold = held.holdAt(func(e provider.ProviderEvent) bool { return e.Kind == provider.EventToolStart })
	s := newHeldSession(t, context.Background(), tuiExitConfig(t, readLineScript("exit 0"), t.TempDir()), rec, held)

	want := []string{"toolu_0"}
	s.feedEnvelope(toolUseEnvelope("toolu_0"))
	held.waitReached(t)
	for i := 1; i <= 20; i++ {
		id := fmt.Sprintf("toolu_%d", i)
		want = append(want, id)
		s.feedEnvelope(toolUseEnvelope(id))
	}
	endProcess(t, s)
	// The teardown is past the gateway's stop, and on to the feed.
	waitRefused(t, addrOf(s.gateway.baseURL()))
	held.open()
	rec.waitDisconnected(t)

	events := rec.snapshot()
	assertLastIsDisconnected(t, events)
	if got := toolStartIDs(events); !slices.Equal(got, want) {
		t.Fatalf("tool starts = %v, want %v", got, want)
	}
}

// stalledUpstream is an upstream that holds every request until the test
// releases it, then drops the connection without answering. arrived closes
// when the first request is held.
type stalledUpstream struct {
	url     string
	arrived chan struct{}
	release func()
}

func newStalledUpstream(t *testing.T) *stalledUpstream {
	t.Helper()
	arrived := make(chan struct{})
	unblock := make(chan struct{})
	var arriveOnce, releaseOnce sync.Once
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arriveOnce.Do(func() { close(arrived) })
		<-unblock
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	})}
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(ln) }()
	release := func() { releaseOnce.Do(func() { close(unblock) }) }
	t.Cleanup(func() {
		release()
		_ = server.Close()
	})
	return &stalledUpstream{url: "http://" + ln.Addr().String(), arrived: arrived, release: release}
}

// stalledGatewaySession starts a session whose gateway holds one request
// the upstream does not answer, so the gateway's stop outlasts its grace.
// The returned channel closes when that request's client got its answer,
// which the gateway sends only once the handler has returned.
func stalledGatewaySession(t *testing.T, ctx context.Context, rec *exitRecorder) (*Session, *stalledUpstream, <-chan struct{}) {
	t.Helper()
	upstream := newStalledUpstream(t)
	cfg := tuiExitConfig(t, readLineScript("exit 0"), t.TempDir())
	cfg.Upstream = upstream.url
	s := newTestSession(t, ctx, cfg, rec)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		resp, err := http.Get(s.gateway.baseURL() + "/v1/models")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	<-upstream.arrived
	return s, upstream, answered
}

const cutOff = "requests still active after 2s were cut off: context deadline exceeded"

// A gateway request that outlasts the stop's grace is cut off, joined, and
// reported once before the teardown pair; when its upstream later fails,
// the handler is long gone and nothing follows "disconnected".
func TestAHandlerPastTheGraceIsCutOffBeforeDisconnected(t *testing.T) {
	rec := newExitRecorder()
	s, upstream, answered := stalledGatewaySession(t, context.Background(), rec)
	endProcess(t, s)
	rec.waitDisconnected(t)

	upstream.release()
	select {
	case <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled request was never answered")
	}
	want := []string{
		string(provider.EventError) + ":release the session: claudetui gateway: " + cutOff,
		string(provider.EventSessionStatus) + ":error",
		string(provider.EventSessionStatus) + ":disconnected",
	}
	if got := eventLines(rec.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if err := s.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close after the cut-off = %v, want its error", err)
	}
}

// A release that fails under Close is Close's error, not a thread event,
// and Close returns after the teardown pair.
func TestCloseReturnsAFailedRelease(t *testing.T) {
	rec := newExitRecorder()
	s, _, _ := stalledGatewaySession(t, context.Background(), rec)
	if err := s.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want the release's error", err)
	}
	if !closed(rec.disconnected) {
		t.Fatal("Close returned before \"disconnected\" was emitted")
	}
	want := []string{string(provider.EventSessionStatus) + ":disconnected"}
	if got := eventLines(rec.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// The context watcher's teardown has nobody waiting on it, so its error is
// posted on the thread before the pair; a later Close returns it too.
func TestTheWatchersFailedReleaseReachesTheThread(t *testing.T) {
	rec := newExitRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, _, _ := stalledGatewaySession(t, ctx, rec)
	cancel()
	rec.waitDisconnected(t)
	want := []string{
		string(provider.EventError) + ":release the session: claudetui gateway: " + cutOff,
		string(provider.EventSessionStatus) + ":disconnected",
	}
	if got := eventLines(rec.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if err := s.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close after the watcher = %v, want the release's error", err)
	}
}

// A PostCompact the relay is still dispatching when the process ends, held
// past the stop's grace, keeps the teardown waiting after its connection is
// cut: the handler is joined before the feed closes and before the
// SessionStart settlement, so its compaction boundary precedes the pair and
// the delivery it armed is forgotten.
func TestAPostCompactInDispatchIsJoinedAndSettled(t *testing.T) {
	rec := newExitRecorder()
	forgot := 0
	cfg := tuiExitConfig(t, readLineScript("exit 0"), t.TempDir())
	cfg.SessionStartHook = testHook()
	cfg.SessionStartForget = func() error { forgot++; return nil }
	cfg.Resume = "sess-1"
	s := newTestSession(t, context.Background(), cfg, rec)
	if got := s.sessionStart.Expecting(); got != "" {
		t.Fatalf("a resume expects %q", got)
	}

	// Hold the relay's PostCompact handler before its Expect.
	s.recMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.recMu.Unlock()
		}
	}()
	posted := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, s.relay.url(), strings.NewReader(`{"hook_event_name":"PostCompact","trigger":"auto","compact_summary":"the summary"}`))
		if err != nil {
			posted <- err
			return
		}
		req.Header.Set(hookAuthHeader, s.relay.authToken())
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		posted <- err
	}()
	waitParked(t, "sync.Mutex.Lock", "agent-overflow/internal/provider/claudetui.(*Session).finalizeCompaction(", 1, posted)

	endProcess(t, s)
	// The stop's grace passed and it closed the hook's connection.
	select {
	case err := <-posted:
		if err == nil {
			t.Fatal("the held PostCompact was answered; want its connection cut")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the relay's stop never cut the held PostCompact")
	}
	waitParked(t, "sync.Cond.Wait", "agent-overflow/internal/provider/claudetui.(*loopbackServer).stop(", 1, nil)
	if closed(rec.disconnected) {
		t.Fatal("\"disconnected\" was emitted while a relay handler was still running")
	}
	s.recMu.Unlock()
	locked = false
	rec.waitDisconnected(t)
	if forgot != 1 {
		t.Fatalf("forgot %d times, want once", forgot)
	}
	want := []string{
		string(provider.EventCompactBoundary),
		string(provider.EventError) + ":release the session: claudetui hook relay: " + cutOff,
		string(provider.EventSessionStatus) + ":error",
		string(provider.EventSessionStatus) + ":disconnected",
	}
	var got []string
	for _, e := range rec.snapshot() {
		if e.Kind == provider.EventCompactBoundary {
			got = append(got, string(e.Kind))
			continue
		}
		got = append(got, fmt.Sprintf("%s:%s", e.Kind, e.Content))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// Close of a session whose process started returns only once the teardown
// pair has reached onEvent: the app drains its event queue right after
// Close and relies on "disconnected" being in it. A second Close waits for
// the same completion.
func TestCloseReturnsAfterTheTeardownPair(t *testing.T) {
	rec := newExitRecorder()
	held := newGate()
	rec.hold = held.holdAt(isDisconnected)
	s := newHeldSession(t, context.Background(), tuiExitConfig(t, readLineScript(""), t.TempDir()), rec, held)

	first := make(chan error, 1)
	go func() { first <- s.Close() }()
	held.waitReached(t)
	waitParked(t, "chan receive", closeFrame, 1, first)
	second := make(chan error, 1)
	go func() { second <- s.Close() }()
	waitParked(t, "chan receive", closeFrame, 2, second)
	if closed(s.Connection().Done()) {
		t.Fatal("the session is complete while \"disconnected\" is being emitted")
	}

	held.open()
	for name, ch := range map[string]chan error{"first": first, "second": second} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s Close: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s Close did not return after the pair was emitted", name)
		}
	}
	assertLastIsDisconnected(t, rec.snapshot())
}

// When the context watcher closes the session first, a caller's later Close
// still returns only after the teardown pair, with the watcher's result.
func TestAWatcherCloseHoldsALaterClose(t *testing.T) {
	rec := newExitRecorder()
	held := newGate()
	rec.hold = held.holdAt(isDisconnected)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newHeldSession(t, ctx, tuiExitConfig(t, readLineScript(""), t.TempDir()), rec, held)

	cancel()
	held.waitReached(t)
	later := make(chan error, 1)
	go func() { later <- s.Close() }()
	// The watcher waits in closeFrom too.
	waitParked(t, "chan receive", closeFrame, 2, later)
	if closed(s.Connection().Done()) {
		t.Fatal("the session is complete while \"disconnected\" is being emitted")
	}
	held.open()
	select {
	case err := <-later:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the pair was emitted")
	}
	if got := statusEvents(rec.snapshot()); !slices.Equal(got, []string{"disconnected"}) {
		t.Fatalf("statuses = %v, want [disconnected]", got)
	}
}

// A self-exit with a SessionStart delivery still pending forgets the
// thread's records once, and a failure to forget reaches the thread before
// the teardown pair. A later Close does not forget again.
func TestASelfExitForgetsAPendingDelivery(t *testing.T) {
	rec := newExitRecorder()
	forgot := 0
	cfg := tuiExitConfig(t, readLineScript("exit 0"), t.TempDir())
	cfg.SessionStartHook = testHook()
	cfg.SessionStartForget = func() error { forgot++; return errors.New("disk full") }
	s := newTestSession(t, context.Background(), cfg, rec)
	endProcess(t, s)
	rec.waitDisconnected(t)
	if forgot != 1 {
		t.Fatalf("forgot %d times, want once", forgot)
	}
	want := []string{
		string(provider.EventError) + ":Agent Overflow could not record that the memory has not reached this session since its start, so a later prompt may deliver only what changed: disk full",
		string(provider.EventSessionStatus) + ":error",
		string(provider.EventSessionStatus) + ":disconnected",
	}
	if got := eventLines(rec.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if forgot != 1 {
		t.Fatalf("Close forgot again: %d times", forgot)
	}
}

// A launch the terminal refuses starts no PTY, so no exit callback will
// complete the session: NewSession's cleanup Close completes it itself and
// returns, with the gateway, relay and prompt files released.
func TestAFailedLaunchCompletesWithoutAPTY(t *testing.T) {
	rec := newExitRecorder()
	promptDir := t.TempDir()
	cfg := tuiExitConfig(t, readLineScript("exit 0"), promptDir)
	cfg.Binary = t.TempDir() + "/does-not-exist"
	result := make(chan error, 1)
	go func() {
		_, err := NewSession(context.Background(), testThread, cfg, rec.record)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("NewSession succeeded without a binary")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("NewSession's cleanup Close waited for a PTY that never started")
	}
	if left := promptFilesIn(t, promptDir); len(left) > 0 {
		t.Errorf("prompt files left behind: %v", left)
	}
	if events := rec.snapshot(); len(events) > 0 {
		t.Fatalf("a failed launch emitted %v", kindsOf(events))
	}
}

// Regression: a cancelled spawn context closes the session through its
// watcher; the host ended it, so the pair is "disconnected" alone.
func TestTheContextWatcherClosesTheSession(t *testing.T) {
	rec := newExitRecorder()
	promptDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTestSession(t, ctx, tuiExitConfig(t, readLineScript(""), promptDir), rec)
	cancel()
	rec.waitDisconnected(t)
	select {
	case <-s.Connection().Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not complete")
	}
	assertReleased(t, s, promptDir)
	if got := statusEvents(rec.snapshot()); !slices.Equal(got, []string{"disconnected"}) {
		t.Fatalf("statuses = %v, want [disconnected]", got)
	}
}

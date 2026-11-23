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
// "disconnected" status arrives.
type exitRecorder struct {
	eventRecorder
	once         sync.Once
	disconnected chan struct{}
}

func newExitRecorder() *exitRecorder {
	return &exitRecorder{disconnected: make(chan struct{})}
}

func (r *exitRecorder) record(e provider.ProviderEvent) {
	r.eventRecorder.record(e)
	if e.Kind == provider.EventSessionStatus && e.Content == "disconnected" {
		r.once.Do(func() { close(r.disconnected) })
	}
}

func (r *exitRecorder) waitDisconnected(t *testing.T) {
	t.Helper()
	select {
	case <-r.disconnected:
	case <-time.After(10 * time.Second):
		t.Fatalf("no \"disconnected\" status; events %v", kindsOf(r.snapshot()))
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

// sessionGoroutineFuncs are the functions a live session's own goroutines
// run in: the feed loop, the context watcher, and the gateway's and relay's
// serve loops.
var sessionGoroutineFuncs = []string{
	"claudetui.(*Session).feedLoop(",
	"claudetui.(*Session).watchContext(",
	"claudetui.(*gateway).start.func1(",
	"claudetui.(*hookRelay).start.func1(",
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

// sessionGoroutines counts the goroutines in each of sessionGoroutineFuncs.
func sessionGoroutines() map[string]int {
	counts := map[string]int{}
	for _, g := range strings.Split(allStacks(), "\n\n") {
		for _, fn := range sessionGoroutineFuncs {
			if strings.Contains(g, fn) {
				counts[fn]++
			}
		}
	}
	return counts
}

// waitSessionGoroutines waits until the session goroutines are back to
// want. A goroutine's return is observable only in the stacks, so this
// polls, bounded.
func waitSessionGoroutines(t *testing.T, want map[string]int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := sessionGoroutines()
		if sameCounts(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session goroutines = %v, want %v\n%s", got, want, allStacks())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sameCounts(a, b map[string]int) bool {
	for _, fn := range sessionGoroutineFuncs {
		if a[fn] != b[fn] {
			return false
		}
	}
	return true
}

func plusOne(base map[string]int) map[string]int {
	out := map[string]int{}
	for _, fn := range sessionGoroutineFuncs {
		out[fn] = base[fn] + 1
	}
	return out
}

// listenerAddrs are the gateway's and the relay's loopback addresses.
func listenerAddrs(s *Session) []string {
	return []string{s.gateway.ln.Addr().String(), s.relay.ln.Addr().String()}
}

func dialable(addr string) bool {
	conn, err := net.Dial("tcp6", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// parserHoldsState reports whether the parser still remembers a tool_use,
// which Parser.Close forgets.
func parserHoldsState(p *claude.Parser) bool {
	return !reflect.ValueOf(p).Elem().FieldByName("toolUseNames").IsZero()
}

const toolUseEnvelope = `{"type":"assistant","message":{"id":"msg_exit","type":"message","role":"assistant","model":"claude-x","content":[{"type":"tool_use","id":"toolu_exit","name":"Bash","input":{"command":"ls"}}]}}`

// assertReleased checks everything a live session holds is gone: both
// listeners refuse connections, the feed loop has returned, done is closed,
// the parser forgot its state, both prompt files are removed, and the
// session's goroutines are back to base.
func assertReleased(t *testing.T, s *Session, addrs []string, promptDir string, base map[string]int) {
	t.Helper()
	for _, addr := range addrs {
		if dialable(addr) {
			t.Errorf("listener %s still accepts connections", addr)
		}
	}
	select {
	case <-s.done:
	default:
		t.Error("done is still open")
	}
	select {
	case <-s.feedDone:
	default:
		t.Error("the feed loop has not returned")
	}
	if parserHoldsState(s.parser) {
		t.Error("parser state was not released")
	}
	if left := promptFilesIn(t, promptDir); len(left) > 0 {
		t.Errorf("prompt files left behind: %v", left)
	}
	waitSessionGoroutines(t, base)
}

// statusEvents is the session-status contents in events, in order.
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
	last := events[len(events)-1]
	if last.Kind != provider.EventSessionStatus || last.Content != "disconnected" {
		t.Fatalf("last event = %v %q, want \"disconnected\"; kinds %v", last.Kind, last.Content, kindsOf(events))
	}
}

// startLiveSession launches script, which reads one line from its terminal
// before it ends, and holds parser state from one fed tool_use. It returns
// the session goroutine counts from before the launch.
func startLiveSession(t *testing.T, ctx context.Context, script, promptDir string, rec *exitRecorder) (*Session, map[string]int) {
	t.Helper()
	base := sessionGoroutines()
	s, err := NewSession(ctx, testThread, tuiExitConfig(t, script, promptDir), rec.record)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if left := promptFilesIn(t, promptDir); len(left) != 2 {
		t.Fatalf("prompt files = %v, want both", left)
	}
	for _, addr := range listenerAddrs(s) {
		if !dialable(addr) {
			t.Fatalf("listener %s refuses connections on a live session", addr)
		}
	}
	waitSessionGoroutines(t, plusOne(base))
	s.feedEnvelope(json.RawMessage(toolUseEnvelope))
	rec.waitForKind(t, provider.EventToolStart)
	if !parserHoldsState(s.parser) {
		t.Fatal("parser holds no state after a tool_use")
	}
	return s, base
}

// A process that ends on its own, cleanly, with an error code, or by a
// signal, releases everything the session held for it with nobody calling
// Close, before its teardown pair, which keeps its events and meta.
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
			s, base := startLiveSession(t, context.Background(), "#!/bin/sh\nread line\n"+tc.ending+"\n", promptDir, rec)
			addrs := listenerAddrs(s)

			if err := s.writePTY([]byte("\n")); err != nil {
				t.Fatalf("write the line: %v", err)
			}
			rec.waitDisconnected(t)
			assertReleased(t, s, addrs, promptDir, base)

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

			// Close after the exit returns what the release returned, and
			// neither Close emits or releases again.
			if err := s.Close(); err != nil {
				t.Fatalf("Close after a self-exit: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}
			if after := rec.snapshot(); len(after) != len(events) {
				t.Fatalf("Close emitted %v after the release", kindsOf(after[len(events):]))
			}
		})
	}
}

// A self-exit and a Close that race release once, emit one teardown pair
// ending in "disconnected", and Close returns no error.
func TestASelfExitRacingCloseReleasesOnce(t *testing.T) {
	rec := newExitRecorder()
	promptDir := t.TempDir()
	s, base := startLiveSession(t, context.Background(), "#!/bin/sh\nread line\nexit 0\n", promptDir, rec)
	addrs := listenerAddrs(s)

	closed := make(chan error, 1)
	if err := s.writePTY([]byte("\n")); err != nil {
		t.Fatalf("write the line: %v", err)
	}
	go func() { closed <- s.Close() }()
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	rec.waitDisconnected(t)
	assertReleased(t, s, addrs, promptDir, base)
	events := rec.snapshot()
	assertLastIsDisconnected(t, events)
	if got := statusEvents(events); strings.Count(strings.Join(got, ","), "disconnected") != 1 {
		t.Fatalf("statuses = %v, want one \"disconnected\"", got)
	}
}

// closeParked reports whether a goroutine is blocked on a channel receive
// in Session.Close itself, which is where Close waits for the teardown pair.
func closeParked() bool {
	for _, g := range strings.Split(allStacks(), "\n\n") {
		header, rest, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[chan receive") && strings.HasPrefix(rest, "agent-overflow/internal/provider/claudetui.(*Session).Close(") {
			return true
		}
	}
	return false
}

// Close of a session whose process started returns only once the teardown
// pair has reached onEvent: the app drains its event queue right after
// Close and relies on "disconnected" being in it.
func TestCloseReturnsAfterTheTeardownPair(t *testing.T) {
	rec := newExitRecorder()
	held := make(chan struct{})
	unhold := make(chan struct{})
	var unholdOnce sync.Once
	t.Cleanup(func() { unholdOnce.Do(func() { close(unhold) }) })
	onEvent := func(e provider.ProviderEvent) {
		if e.Kind == provider.EventSessionStatus && e.Content == "disconnected" {
			close(held)
			<-unhold
		}
		rec.record(e)
	}
	s, err := NewSession(context.Background(), testThread, tuiExitConfig(t, "#!/bin/sh\nread line\n", t.TempDir()), onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the teardown pair was never emitted")
	}
	// Close's goroutine has nothing left to wait for but the pair, so it is
	// either parked on it or gone. Seeing it parked is the bounded poll.
	deadline := time.Now().Add(5 * time.Second)
	for !closeParked() {
		select {
		case err := <-closed:
			t.Fatalf("Close returned (%v) while \"disconnected\" was still being emitted", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Close is not waiting for the teardown pair\n%s", allStacks())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A second Close does not wait behind the first.
	second := make(chan error, 1)
	go func() { second <- s.Close() }()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second Close while the first waits: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second Close waited behind the first")
	}

	unholdOnce.Do(func() { close(unhold) })
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the pair was emitted")
	}
	assertLastIsDisconnected(t, rec.snapshot())
}

// A cancelled spawn context closes the session through its watcher: the
// host ended it, so the pair is "disconnected" alone.
func TestTheContextWatcherClosesTheSession(t *testing.T) {
	rec := newExitRecorder()
	promptDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, base := startLiveSession(t, ctx, "#!/bin/sh\nread line\n", promptDir, rec)
	addrs := listenerAddrs(s)

	cancel()
	rec.waitDisconnected(t)
	assertReleased(t, s, addrs, promptDir, base)
	events := rec.snapshot()
	assertLastIsDisconnected(t, events)
	if got := statusEvents(events); !slices.Equal(got, []string{"disconnected"}) {
		t.Fatalf("statuses = %v, want [disconnected]", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close after the watcher's: %v", err)
	}
}

// A process that exits as soon as it starts, possibly while NewSession is
// still setting up, releases the session too, and a later Close is a
// no-op.
func TestAnExitDuringSetupReleasesTheSession(t *testing.T) {
	rec := newExitRecorder()
	promptDir := t.TempDir()
	base := sessionGoroutines()
	s, err := NewSession(context.Background(), testThread, tuiExitConfig(t, "#!/bin/sh\nexit 0\n", promptDir), rec.record)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	addrs := listenerAddrs(s)
	rec.waitDisconnected(t)
	select {
	case <-s.done:
	default:
		t.Error("done is still open")
	}
	for _, addr := range addrs {
		if dialable(addr) {
			t.Errorf("listener %s still accepts connections", addr)
		}
	}
	if left := promptFilesIn(t, promptDir); len(left) > 0 {
		t.Errorf("prompt files left behind: %v", left)
	}
	waitSessionGoroutines(t, base)
	assertLastIsDisconnected(t, rec.snapshot())
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// A launch that fails after the gateway and relay started releases them and
// the prompt files, and emits nothing: no process ran.
func TestAFailedLaunchReleasesTheSession(t *testing.T) {
	rec := newExitRecorder()
	promptDir := t.TempDir()
	base := sessionGoroutines()
	cfg := tuiExitConfig(t, "#!/bin/sh\nexit 0\n", promptDir)
	cfg.Binary = t.TempDir() + "/does-not-exist"
	if _, err := NewSession(context.Background(), testThread, cfg, rec.record); err == nil {
		t.Fatal("NewSession succeeded without a binary")
	}
	if left := promptFilesIn(t, promptDir); len(left) > 0 {
		t.Errorf("prompt files left behind: %v", left)
	}
	waitSessionGoroutines(t, base)
	if events := rec.snapshot(); len(events) > 0 {
		t.Fatalf("a failed launch emitted %v", kindsOf(events))
	}
}

// A self-exit with a SessionStart delivery still pending forgets the
// thread's records once, and a failure to forget reaches the thread before
// the teardown pair. A later Close does not forget again.
func TestASelfExitForgetsAPendingDelivery(t *testing.T) {
	rec := newExitRecorder()
	promptDir := t.TempDir()
	forgot := 0
	cfg := tuiExitConfig(t, "#!/bin/sh\nread line\nexit 0\n", promptDir)
	cfg.SessionStartHook = testHook()
	cfg.SessionStartForget = func() error { forgot++; return errors.New("disk full") }
	s, err := NewSession(context.Background(), testThread, cfg, rec.record)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.writePTY([]byte("\n")); err != nil {
		t.Fatalf("write the line: %v", err)
	}
	rec.waitDisconnected(t)
	if forgot != 1 {
		t.Fatalf("forgot %d times, want once", forgot)
	}
	var kinds []string
	for _, e := range rec.snapshot() {
		kinds = append(kinds, fmt.Sprintf("%s:%s", e.Kind, firstLine(e.Content)))
	}
	want := []string{
		string(provider.EventError) + ":Agent Overflow could not record that the memory has not reached this session since its start, so a later prompt may deliver only what changed: disk full",
		string(provider.EventSessionStatus) + ":error",
		string(provider.EventSessionStatus) + ":disconnected",
	}
	if !slices.Equal(kinds, want) {
		t.Fatalf("events = %q, want %q", kinds, want)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if forgot != 1 {
		t.Fatalf("Close forgot again: %d times", forgot)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// stalledGatewaySession starts a session whose gateway holds a request the
// upstream does not answer until the test ends, so closing the gateway
// outlasts its shutdown grace and the release fails.
func stalledGatewaySession(t *testing.T, rec *exitRecorder) *Session {
	t.Helper()
	arrived := make(chan struct{})
	unblock := make(chan struct{})
	upstream := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-unblock
	})}
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = upstream.Serve(ln) }()
	t.Cleanup(func() {
		close(unblock)
		_ = upstream.Close()
	})

	cfg := tuiExitConfig(t, "#!/bin/sh\nread line\nexit 0\n", t.TempDir())
	cfg.Upstream = "http://" + ln.Addr().String()
	s, err := NewSession(context.Background(), testThread, cfg, rec.record)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	go func() {
		resp, err := http.Get(s.gateway.baseURL() + "/v1/models")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	<-arrived
	return s
}

func eventLines(events []provider.ProviderEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s:%s", e.Kind, e.Content))
	}
	return out
}

// A release that fails after a self-exit reaches the thread as an error
// before the teardown pair, since no Close caller would see it.
func TestASelfExitReportsAFailedRelease(t *testing.T) {
	rec := newExitRecorder()
	s := stalledGatewaySession(t, rec)
	if err := s.writePTY([]byte("\n")); err != nil {
		t.Fatalf("write the line: %v", err)
	}
	rec.waitDisconnected(t)

	want := []string{
		string(provider.EventError) + ":release the session after its process ended: " + context.DeadlineExceeded.Error(),
		string(provider.EventSessionStatus) + ":error",
		string(provider.EventSessionStatus) + ":disconnected",
	}
	if got := eventLines(rec.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if err := s.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close after the failed release = %v, want its error", err)
	}
}

// A release that fails under Close is Close's error, not a thread event.
func TestCloseReturnsAFailedRelease(t *testing.T) {
	rec := newExitRecorder()
	s := stalledGatewaySession(t, rec)
	if err := s.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want the release's error", err)
	}
	select {
	case <-rec.disconnected:
	default:
		t.Fatal("Close returned before \"disconnected\" was emitted")
	}
	want := []string{string(provider.EventSessionStatus) + ":disconnected"}
	if got := eventLines(rec.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

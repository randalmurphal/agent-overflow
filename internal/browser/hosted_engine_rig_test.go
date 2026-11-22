package browser

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
)

// The hosted engine is exercised against a fake directive sink and a fake
// report source. Nothing here reaches an external launcher, a WebView2
// runtime, or a CDP endpoint outside the test: the relay is a stub, the
// CDP browsers are in-process fakes, and every failure path the tests drive is one
// the engine must answer with an error rather than a hang.

const hostedTestDeadline = 3 * time.Second

// directiveSink is the launcher's inbox.
type directiveSink struct {
	mu   sync.Mutex
	all  []webview2host.Directive
	feed chan webview2host.Directive
}

func newDirectiveSink() *directiveSink {
	return &directiveSink{feed: make(chan webview2host.Directive, 64)}
}

func (s *directiveSink) send(directive webview2host.Directive) {
	s.mu.Lock()
	s.all = append(s.all, directive)
	s.mu.Unlock()
	select {
	case s.feed <- directive:
	default:
	}
}

func (s *directiveSink) ops() []webview2host.Op {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops := make([]webview2host.Op, 0, len(s.all))
	for _, directive := range s.all {
		ops = append(ops, directive.Op)
	}
	return ops
}

func (s *directiveSink) next(t *testing.T) webview2host.Directive {
	t.Helper()
	select {
	case directive := <-s.feed:
		return directive
	case <-time.After(hostedTestDeadline):
		t.Fatal("timed out waiting for a directive")
	}
	return webview2host.Directive{}
}

// drain answers the directives sent since the last next or drain.
func (s *directiveSink) drain() []webview2host.Directive {
	var out []webview2host.Directive
	for {
		select {
		case directive := <-s.feed:
			out = append(out, directive)
		default:
			return out
		}
	}
}

// expectOps asserts the exact directive sequence, which is the whole
// observable contract of a failed create: ask, then clean up.
func (s *directiveSink) expectOps(t *testing.T, want ...webview2host.Op) {
	t.Helper()
	got := s.ops()
	if len(got) != len(want) {
		t.Fatalf("directives %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("directives %v, want %v", got, want)
		}
	}
}

type stubRelay struct {
	url string
	err error
}

func (r stubRelay) BrowserWebSocketURL(context.Context) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.url, nil
}

func newTestHostedEngine(t *testing.T, relay hostRelay, events engineEvents) (*hostedEngine, *directiveSink) {
	t.Helper()
	sink := newDirectiveSink()
	if events.PageEndReceived == nil {
		// The Manager's popup admission, which these engines have none of.
		events.PageEndReceived = func(ids.PageHandle, bool, string, []ids.PageHandle) {}
	}
	engine := newHostedEngine(relay, sink.send, nil, events)
	engine.logf = func(format string, args ...any) { t.Logf(format, args...) }
	engine.createTimeout = 150 * time.Millisecond
	engine.attachTimeout = 150 * time.Millisecond
	engine.ownerWait = 150 * time.Millisecond
	engine.clearTimeout = 150 * time.Millisecond
	engine.windowsPath = testWindowsPath
	return engine, sink
}

// testWindowsPath stands in for `wslpath -w`.
func testWindowsPath(_ context.Context, path string) (string, error) {
	return `\\wsl.localhost\Test` + strings.ReplaceAll(path, "/", `\`), nil
}

func testProfile(t *testing.T, engine *hostedEngine) *hostedProfile {
	t.Helper()
	profile, err := engine.NewProfile(context.Background(), profileOptions{Jar: "/home/dev/repo", Ephemeral: true})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	return profile.(*hostedProfile)
}

// answer replies to the next create directive with one report, which is
// what a launcher does over the notification bridge.
func answerCreate(t *testing.T, engine *hostedEngine, sink *directiveSink, kind webview2host.ReportKind, detail string) {
	t.Helper()
	go func() {
		directive := sink.next(t)
		if directive.Op != webview2host.OpCreate {
			t.Errorf("first directive was %q, want create", directive.Op)
			return
		}
		engine.Report(ids.PageHandle(directive.PageID), kind, detail)
	}()
}

// answerClear replies to the next clear-data directive, which is what the
// launcher does once it has closed every controller, released the
// environment, and finished deleting its user-data folder.
// It hands back the directive it answered, because the feed is consumed by
// the reply and a caller cannot read it twice.
func answerClear(t *testing.T, engine *hostedEngine, sink *directiveSink, kind webview2host.ReportKind, detail string) <-chan webview2host.Directive {
	t.Helper()
	seen := make(chan webview2host.Directive, 1)
	go func() {
		directive := sink.next(t)
		if directive.Op != webview2host.OpClearData {
			t.Errorf("first directive was %q, want clear-data", directive.Op)
			return
		}
		seen <- directive
		engine.Report(ids.PageHandle(directive.PageID), kind, detail)
	}()
	return seen
}

// bindTestPage binds a page on no browser connection, one that never ends.
func bindTestPage(t *testing.T, engine *hostedEngine, pageID ids.PageHandle, targetID target.ID) {
	t.Helper()
	if err := bindForTest(engine, pageID, targetID, context.Background()); err != nil {
		t.Fatal(err)
	}
}

// pendingTestPopup records popup as the launcher's report of it does
// (popupReported): opened by opener, in its profile, admitting downloads, and
// not yet attached or listed.
func pendingTestPopup(engine *hostedEngine, popup ids.PageHandle, targetID target.ID, opener ids.PageHandle) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	profile := engine.pageProfile[opener]
	engine.popups[popup] = &hostedPopup{target: targetID, profile: profile, opener: opener}
	engine.popupByTarget[targetID] = popup
	engine.pageProfile[popup] = profile
	engine.pages.addPopup(popup, opener, profile)
}

package backendproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// carrierFor is a carrier of upstream with its windows shortened.
func carrierFor(t *testing.T, upstream *httptest.Server, stall, allowance time.Duration) *Carrier {
	t.Helper()
	c, err := New(Config{
		WSURL: "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws",
		Token: "upstream-launch-token",
	})
	if err != nil {
		t.Fatalf("new carrier: %v", err)
	}
	c.stall, c.allowance = stall, allowance
	return c
}

// relayOf serves c as the relay a page reaches. It closes after every
// connection a test opened to it, so a relay still blocked on one fails
// the test rather than hanging it.
func relayOf(t *testing.T, c *Carrier) *httptest.Server {
	t.Helper()
	relay := httptest.NewServer(http.HandlerFunc(c.CarryTransfer))
	t.Cleanup(relay.Close)
	return relay
}

// sendRaw writes head and body to the relay over a connection of its own,
// which then sends nothing more, and answers it.
func sendRaw(t *testing.T, relay *httptest.Server, head string, body []byte) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(relay.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write(append([]byte(head), body...)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// readAnswer reads the whole answer on conn within within.
func readAnswer(t *testing.T, conn net.Conn, within time.Duration, what string) (*http.Response, string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("%s: no answer within %s: %v", what, within, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: the answer's body: %v", what, err)
	}
	return resp, string(body)
}

// TestCarryTransfer_AnEarlyAnswerReachesAStalledPage — a page that sent half
// its body and stopped gets the answer the upstream gave early, whole, at
// once: the relay does not wait out the body's read.
func TestCarryTransfer_AnEarlyAnswerReachesAStalledPage(t *testing.T) {
	const answer = `{"parts":[{"state":"not_sent"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadFull(r.Body, make([]byte, 5)); err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(upstream.Close)
	relay := relayOf(t, carrierFor(t, upstream, time.Minute, time.Minute))
	conn := sendRaw(t, relay, "PUT /attachments/tracker/upload?ticket=t HTTP/1.1\r\nHost: relay\r\nContent-Length: 100000\r\n\r\n", make([]byte, 50000))
	resp, body := readAnswer(t, conn, 5*time.Second, "an early answer to a stalled page")
	if resp.StatusCode != http.StatusOK || body != answer {
		t.Fatalf("an early answer to a stalled page: %d %q", resp.StatusCode, body)
	}
}

// TestCarryTransfer_APageThatStopsSendingIsAnswered — a page that stops
// sending is answered once the stall window passes, with what happened and
// what to do, and the upstream request ends with it.
func TestCarryTransfer_APageThatStopsSendingIsAnswered(t *testing.T) {
	ended := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(upstream.Close)
	relay := relayOf(t, carrierFor(t, upstream, 2*time.Second, time.Minute))
	conn := sendRaw(t, relay, "PUT /attachments/upload?ticket=t HTTP/1.1\r\nHost: relay\r\nContent-Length: 65536\r\n\r\n", make([]byte, 32768))
	resp, body := readAnswer(t, conn, 8*time.Second, "a page that stopped sending")
	if resp.StatusCode != http.StatusRequestTimeout || !strings.Contains(body, "the file stopped arriving at this computer for 2 seconds, so it was not sent on in full; send it again") {
		t.Fatalf("a page that stopped sending: %d %q", resp.StatusCode, body)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request outlived the page's stall")
	}
}

// TestCarryTransfer_AnUpstreamThatWithholdsItsHeaders — an upstream that
// takes the request and never begins its answer is ended at the answer
// allowance, and the page is told.
func TestCarryTransfer_AnUpstreamThatWithholdsItsHeaders(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	defer close(release)
	relay := relayOf(t, carrierFor(t, upstream, time.Minute, 2*time.Second))
	conn := sendRaw(t, relay, "GET /attachments/tracker/501?ticket=t HTTP/1.1\r\nHost: relay\r\n\r\n", nil)
	resp, body := readAnswer(t, conn, 8*time.Second, "an upstream withholding its headers")
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(body, "the computer that holds it did not begin to answer within 2 seconds; try again") {
		t.Fatalf("an upstream withholding its headers: %d %q", resp.StatusCode, body)
	}
}

// TestCarryTransfer_AnUpstreamThatStopsMidAnswer — an upstream that sends its
// headers and one chunk and then nothing has the page's answer cut once the
// stall window passes.
func TestCarryTransfer_AnUpstreamThatStopsMidAnswer(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	defer close(release)
	relay := relayOf(t, carrierFor(t, upstream, 2*time.Second, time.Minute))
	conn := sendRaw(t, relay, "GET /attachments/tracker/501?ticket=t HTTP/1.1\r\nHost: relay\r\n\r\n", nil)
	if err := conn.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no headers: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	var timeout net.Error
	if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("an upstream that stopped after %d bytes: the answer was not cut within 8s (%v)", len(got), err)
	}
}

// TestCarryTransfer_AnUploadOfNoDeclaredLength — an upload that declares no
// length is bounded by its progress alone, and its answer has the allowance
// from its last byte: one that moves for longer than both windows together
// and is answered late still reaches the page.
func TestCarryTransfer_AnUploadOfNoDeclaredLength(t *testing.T) {
	const answer = `{"parts":[{"state":"applied"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		time.Sleep(1500 * time.Millisecond)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(upstream.Close)
	relay := relayOf(t, carrierFor(t, upstream, 2*time.Second, 2*time.Second))
	pr, pw := io.Pipe()
	go func() {
		for range 10 {
			if _, err := pw.Write(make([]byte, 1024)); err != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		_ = pw.Close()
	}()
	req, err := http.NewRequest(http.MethodPut, relay.URL+"/attachments/tracker/upload?ticket=t", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("an upload of no declared length moving for 5s: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != answer {
		t.Fatalf("an upload of no declared length: %d %q %v", resp.StatusCode, body, err)
	}
}

// TestCarryTransfer_AnUpstreamThatStopsTakingTheBody — an upstream that stops
// reading an upload leaves no read of the page waiting; the stall window
// still ends the transfer, and the page is told which side stopped.
func TestCarryTransfer_AnUpstreamThatStopsTakingTheBody(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	defer close(release)
	relay := relayOf(t, carrierFor(t, upstream, 2*time.Second, time.Minute))
	conn := sendRaw(t, relay, "PUT /attachments/upload?ticket=t HTTP/1.1\r\nHost: relay\r\nContent-Length: 1073741824\r\n\r\n", nil)
	// The page keeps sending until the relay stops reading it; the
	// upstream's socket buffers fill first.
	go func() {
		chunk := make([]byte, 64<<10)
		for {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	}()
	resp, body := readAnswer(t, conn, 10*time.Second, "an upstream that stopped taking the body")
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(body, "the computer that holds it stopped taking the file for 2 seconds, so it did not receive it in full; send it again") {
		t.Fatalf("an upstream that stopped taking the body: %d %q", resp.StatusCode, body)
	}
}

// TestCarryTransfer_AnUploadSentWholeWhoseAnswerIsWithheld — an upstream
// that took the whole file and never answers may have stored it, so the
// page is told it may have arrived and to check before sending it again,
// never to send it again outright.
func TestCarryTransfer_AnUploadSentWholeWhoseAnswerIsWithheld(t *testing.T) {
	received := make(chan int, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return
		}
		received <- int(n)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	relay := relayOf(t, carrierFor(t, upstream, time.Minute, 2*time.Second))
	conn := sendRaw(t, relay, "PUT /attachments/tracker/upload?ticket=t HTTP/1.1\r\nHost: relay\r\nContent-Length: 5\r\n\r\n", []byte("hello"))
	resp, body := readAnswer(t, conn, 8*time.Second, "an upload sent whole whose answer is withheld")
	if n := <-received; n != 5 {
		t.Fatalf("the upstream received %d bytes", n)
	}
	if resp.StatusCode != http.StatusGatewayTimeout ||
		!strings.Contains(body, "the file was sent on in full, but the computer that holds it did not begin to answer within 2 seconds, so it may have been received; check before sending it again") {
		t.Fatalf("an upload sent whole whose answer is withheld: %d %q", resp.StatusCode, body)
	}
}

// TestCarryTransfer_AnUploadSentWholeWhoseConnectionCloses — an upstream
// that took the whole file and closed its connection before answering may
// have stored it: the page is told so, not that the computer was
// unreachable.
func TestCarryTransfer_AnUploadSentWholeWhoseConnectionCloses(t *testing.T) {
	received := make(chan int64, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return
		}
		received <- n
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(upstream.Close)
	relay := relayOf(t, carrierFor(t, upstream, time.Minute, time.Minute))
	conn := sendRaw(t, relay, "PUT /attachments/tracker/upload?ticket=t HTTP/1.1\r\nHost: relay\r\nContent-Length: 5\r\n\r\n", []byte("hello"))
	resp, body := readAnswer(t, conn, 8*time.Second, "an upload sent whole whose connection closed")
	if n := <-received; n != 5 {
		t.Fatalf("the upstream received %d bytes", n)
	}
	if resp.StatusCode != http.StatusBadGateway ||
		body != "the file was sent on in full, but the connection to the computer that holds it ended before it answered, so it may have been received; check before sending it again\n" {
		t.Fatalf("an upload sent whole whose connection closed: %d %q", resp.StatusCode, body)
	}
}

// TestCarryTransfer_AnEmptyUploadWhoseAnswerIsWithheld — an empty upload
// is whole at once: one the upstream took and never answered may have
// been stored, and the page is told to check, not to try again.
func TestCarryTransfer_AnEmptyUploadWhoseAnswerIsWithheld(t *testing.T) {
	asked := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.Method
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	relay := relayOf(t, carrierFor(t, upstream, time.Minute, 2*time.Second))
	conn := sendRaw(t, relay, "PUT /attachments/tracker/upload?ticket=t HTTP/1.1\r\nHost: relay\r\nContent-Length: 0\r\n\r\n", nil)
	resp, body := readAnswer(t, conn, 8*time.Second, "an empty upload whose answer is withheld")
	if m := <-asked; m != http.MethodPut {
		t.Fatalf("the upstream was asked %s", m)
	}
	if resp.StatusCode != http.StatusGatewayTimeout ||
		body != "the file was sent on in full, but the computer that holds it did not begin to answer within 2 seconds, so it may have been received; check before sending it again\n" {
		t.Fatalf("an empty upload whose answer is withheld: %d %q", resp.StatusCode, body)
	}
}

// TestCarryTransfer_AnUnreachableUpstream — a dial that fails sent nothing:
// an upload is told so and to send it again, a download keeps its words.
func TestCarryTransfer_AnUnreachableUpstream(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	c := carrierFor(t, gone, time.Minute, time.Minute)
	gone.Close()
	for _, tc := range []struct {
		method string
		body   string
		status int
		want   string
	}{
		{http.MethodPut, "hello", http.StatusServiceUnavailable, "the computer that holds it could not be reached, so nothing was sent; send it again\n"},
		{http.MethodGet, "", http.StatusServiceUnavailable, "backend unreachable\n"},
	} {
		rec := httptest.NewRecorder()
		c.CarryTransfer(rec, httptest.NewRequest(tc.method, "/attachments/upload?ticket=t", strings.NewReader(tc.body)))
		if rec.Code != tc.status || rec.Body.String() != tc.want {
			t.Errorf("%s to an upstream that is gone: %d %q", tc.method, rec.Code, rec.Body)
		}
	}
}

// TestCarryTransfer_AServerShuttingDownEndsAStalledUpload — the request's
// context ending (transport's BaseContext at shutdown) while net/http's
// transport is blocked reading a page that stopped sending ends that read
// at once, so the shutdown's join does not wait out the stall window.
// The cancel comes once the upstream has received bytes, so the transport
// is reading the page when it does.
func TestCarryTransfer_AServerShuttingDownEndsAStalledUpload(t *testing.T) {
	got := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadFull(r.Body, make([]byte, 1)); err != nil {
			return
		}
		got <- struct{}{}
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(upstream.Close)
	c := carrierFor(t, upstream, time.Minute, time.Minute)
	root, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	returned := make(chan struct{})
	relay := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(returned)
		c.CarryTransfer(w, r)
	}))
	relay.Config.BaseContext = func(net.Listener) context.Context { return root }
	relay.Start()
	t.Cleanup(relay.Close)
	sendRaw(t, relay, "PUT /attachments/upload?ticket=t HTTP/1.1\r\nHost: relay\r\nContent-Length: 65536\r\n\r\n", make([]byte, 1024))
	<-got
	shutdown()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled upload outlived the server's context by 5s")
	}
}

// gatedDeadlines is a writer whose read deadline, once set to end a read,
// blocks until released, and records a deadline set after the handler
// using it returned.
type gatedDeadlines struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	once             sync.Once
	returned, late   atomic.Bool
}

func (g *gatedDeadlines) SetReadDeadline(d time.Time) error {
	if !d.IsZero() {
		g.once.Do(func() { close(g.entered) })
		<-g.release
		if g.returned.Load() {
			g.late.Store(true)
		}
	}
	return nil
}

func (g *gatedDeadlines) SetWriteDeadline(time.Time) error { return nil }

// heldTrip is a round trip that does not return before until is closed.
type heldTrip struct {
	inner http.RoundTripper
	until <-chan struct{}
}

func (h heldTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := h.inner.RoundTrip(r)
	<-h.until
	return resp, err
}

// TestCarryTransfer_ACarryReturnsAfterItsLeaveCallback — the request's
// context ending begins a callback that ends the page's read; the carry
// does not return while that callback runs, so nothing sets a deadline on
// a connection the handler gave back.
func TestCarryTransfer_ACarryReturnsAfterItsLeaveCallback(t *testing.T) {
	asked := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	c := carrierFor(t, upstream, time.Minute, time.Minute)
	g := &gatedDeadlines{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	// The upstream round trip, which the request's end also cancels, is
	// held until the callback entered: a round trip that returned first
	// lets the carry finish before the callback begins, and nothing would
	// be left to join.
	c.bytes.Transport = heldTrip{inner: http.DefaultTransport, until: g.entered}
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.CarryTransfer(g, httptest.NewRequest(http.MethodGet, "/attachments/tracker/501?ticket=t", nil).WithContext(ctx))
		g.returned.Store(true)
	}()
	<-asked
	leave()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		close(g.release)
		t.Fatal("the request's end did not end the page's read")
	}
	// An absence: the carry must still be waiting on the callback. A
	// carry that does not wait returns within this in every run.
	select {
	case <-done:
		close(g.release)
		t.Fatal("the carry returned while its leave callback was still setting the page's deadline")
	case <-time.After(300 * time.Millisecond):
	}
	close(g.release)
	<-done
	if g.late.Load() {
		t.Fatal("a deadline was set on the page after the carry returned")
	}
}

// TestCarryTransfer_PageBytesDoNotExtendAStalledAnswer — once the upstream's
// headers arrived, only its own bytes restart the window: a full-duplex
// upstream that answered and stopped sending while it still reads the
// file has the page's answer cut, though the page keeps sending.
func TestCarryTransfer_PageBytesDoNotExtendAStalledAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.EnableFullDuplex(); err != nil {
			t.Errorf("full duplex: %v", err)
			return
		}
		if _, err := io.ReadFull(r.Body, make([]byte, 1)); err != nil {
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("x"))
		_ = rc.Flush()
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(upstream.Close)
	relay := relayOf(t, carrierFor(t, upstream, 2*time.Second, time.Minute))
	conn := sendRaw(t, relay, "PUT /attachments/upload?ticket=t HTTP/1.1\r\nHost: relay\r\nContent-Length: 1048576\r\n\r\n", make([]byte, 64<<10))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
			if _, err := conn.Write(make([]byte, 1024)); err != nil {
				return
			}
		}
	}()
	if err := conn.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no headers: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	var timeout net.Error
	if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("an answer that stopped after %d bytes while the page kept sending: not cut within 8s (%v)", len(got), err)
	}
}

// TestCarryTransfer_APageThatCannotSetDeadlines — a writer with no read
// deadline still has the upstream bounded, and says so once.
func TestCarryTransfer_APageThatCannotSetDeadlines(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	saved := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(saved) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(upstream.Close)
	c := carrierFor(t, upstream, time.Minute, time.Second)
	for range 2 {
		rec := httptest.NewRecorder()
		c.CarryTransfer(rec, httptest.NewRequest(http.MethodGet, "/attachments/tracker/501?ticket=t", nil))
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("an upstream withholding its headers to a writer with no deadlines: %d %q", rec.Code, rec.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(logged.String(), "cannot end a blocked read of the page"); n != 1 {
		t.Fatalf("logged %d times: %q", n, logged.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// The timer's firing and a phase switch exclude each other: a fire that
// meets the body's end or the upstream's headers either ends the transfer
// with the phase it fired in, or finds the window restarted and does
// nothing.
func TestCarriedTransferFireRacesAPhaseSwitch(t *testing.T) {
	for _, sw := range []struct {
		name  string
		do    func(*carriedTransfer)
		after phase
	}{
		{"the body's end", (*carriedTransfer).bodyEnded, phaseAwaiting},
		{"the upstream's headers", (*carriedTransfer).answered, phaseAnswering},
	} {
		for range 200 {
			c := &Carrier{stall: time.Minute, allowance: time.Minute}
			ctx, cancel := context.WithCancelCause(context.Background())
			tr := &carriedTransfer{c: c, rc: http.NewResponseController(httptest.NewRecorder()), cancel: cancel,
				phase: phaseBody, window: time.Millisecond, moved: time.Now().Add(-time.Second)}
			tr.reading.Store(true)
			tr.timer = time.AfterFunc(time.Hour, func() {})
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Go(func() { <-start; tr.fire() })
			wg.Go(func() { <-start; sw.do(tr) })
			close(start)
			wg.Wait()
			tr.mu.Lock()
			p, ended := tr.phase, tr.ended
			tr.mu.Unlock()
			switch {
			case p == phaseDone && errors.Is(context.Cause(ctx), errTimerEnded) && ended == endPageStalled:
			case p == sw.after && ctx.Err() == nil:
				tr.fire() // a stale fire after the switch does nothing
				if ctx.Err() != nil {
					t.Fatalf("%s: a fire after the switch ended the transfer", sw.name)
				}
			default:
				t.Fatalf("%s: phase %d, cause %v", sw.name, p, context.Cause(ctx))
			}
			tr.timer.Stop()
		}
	}
}

// Every way a carry ends before the upstream's headers is told by failed,
// and each sentence is true of what may have happened: an upload sent on
// whole (an empty one included) says it may have been received and to
// check, one not sent on whole to send it again, and anything else keeps
// a download's words (plan: what was sent, against how the carry ended).
func TestTheRelaysSentencesAreTrue(t *testing.T) {
	const check = "so it may have been received; check before sending it again"
	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	for _, tc := range []struct {
		name                         string
		ended                        ending
		upload, whole, reached, left bool
		status                       int
		want                         string
	}{
		{"a page that stopped sending", endPageStalled, true, false, true, false, http.StatusRequestTimeout, "the file stopped arriving at this computer for 60 seconds, so it was not sent on in full; send it again"},
		{"an upstream that stopped taking the body", endUpstreamStalled, true, false, true, false, http.StatusGatewayTimeout, "the computer that holds it stopped taking the file for 60 seconds, so it did not receive it in full; send it again"},
		{"a download not answered", endUnanswered, false, true, true, false, http.StatusGatewayTimeout, "the computer that holds it did not begin to answer within 60 seconds; try again"},
		{"an upload sent whole not answered", endUnanswered, true, true, true, false, http.StatusGatewayTimeout, "the file was sent on in full, but the computer that holds it did not begin to answer within 60 seconds, " + check},
		{"a download whose connection ended", endNone, false, true, true, false, http.StatusServiceUnavailable, "backend unreachable"},
		{"an upload whose connection ended mid-body", endNone, true, false, true, false, http.StatusBadGateway, "the connection to the computer that holds it ended before the file was sent on in full; send it again"},
		{"an upload sent whole whose connection ended", endNone, true, true, true, false, http.StatusBadGateway, "the file was sent on in full, but the connection to the computer that holds it ended before it answered, " + check},
		{"a download that could not reach it", endNone, false, true, false, false, http.StatusServiceUnavailable, "backend unreachable"},
		{"an upload that could not reach it", endNone, true, false, false, false, http.StatusServiceUnavailable, "the computer that holds it could not be reached, so nothing was sent; send it again"},
		{"an empty upload that could not reach it", endNone, true, true, false, false, http.StatusServiceUnavailable, "the computer that holds it could not be reached, so nothing was sent; send it again"},
		{"a download whose page left", endNone, false, true, true, true, http.StatusServiceUnavailable, "backend unreachable"},
		{"an upload whose page left mid-body", endNone, true, false, true, true, http.StatusServiceUnavailable, "this computer stopped before the file was sent on in full; send it again"},
		{"an upload sent whole whose page left", endNone, true, true, true, true, http.StatusServiceUnavailable, "the file was sent on in full, but this computer stopped waiting for its answer, " + check},
	} {
		tr := &carriedTransfer{ended: tc.ended, upload: tc.upload, whole: tc.whole, window: time.Minute, page: context.Background()}
		if tc.left {
			tr.page = gone
		}
		tr.reached.Store(tc.reached)
		if status, message := tr.failed(); status != tc.status || message != tc.want {
			t.Errorf("%s: %d %q, want %d %q", tc.name, status, message, tc.status, tc.want)
		}
	}
}

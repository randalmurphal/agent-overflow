package backendproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/transport"
)

// A carried transfer's timing has one rule, owned by carriedTransfer: it
// ends when no byte moves for the stall window in the direction the phase
// is waiting on, except that the upstream's answer has the answer
// allowance from the request's last byte to its own first byte. Nothing here is
// sized from a declared length, and nothing relies on a dialer's
// keepalive or on the backend's own timers: the backend may be another
// computer on another version.
//
// The phases, each switched under one lock:
//
//   - the body in flight: each byte read from the page restarts the stall
//     window;
//   - the answer awaited: from the body's end, or at once for a request
//     with no body, the answer allowance;
//   - the answer in flight: from the upstream's headers, each byte read
//     from the upstream restarts the stall window, and a page byte still
//     going up does not; the page's side of it is the moving write
//     deadline (transport.WriteByProgress);
//   - done: the carry returned.
//
// One timer rather than a moving read deadline on the page: an upstream
// that stops reading the body leaves no page read pending for a deadline
// to fail, and the timer still fires.
//
// What the page is told when a carry ends before the upstream's headers
// is true of what may have happened, and one function says it (failed):
// an upload that did not go out whole cannot have been applied, so it
// says to send it again; one sent on whole, an empty one included, may
// have been, so it says to check first; anything else keeps the words a
// download always had.

// carriedKey carries a request's carriedTransfer to the proxy's
// ModifyResponse and ErrorHandler.
type carriedKey struct{}

type phase uint8

const (
	phaseBody phase = iota
	phaseAwaiting
	phaseAnswering
	phaseDone
)

// ending is how the timer ended a carry; failed words it.
type ending uint8

const (
	// endNone: the timer did not end it, or ended it after the upstream's
	// headers, when the page's answer is cut and nothing is written.
	endNone ending = iota
	endPageStalled
	endUpstreamStalled
	endUnanswered
)

// errTimerEnded is the cause the timer cancels the upstream request with.
var errTimerEnded = errors.New("the carried transfer's timer ended it")

// carriedTransfer is one carried request's timing.
type carriedTransfer struct {
	c      *Carrier
	rc     *http.ResponseController
	cancel context.CancelCauseFunc
	// page is the request's own context, which ends when the page leaves
	// or this server shuts down.
	page context.Context
	// upload is set for a request that may change what the upstream
	// holds: any method but GET and HEAD, whatever its length.
	upload bool
	// reached is set once the request's headers went out to the upstream:
	// before that, nothing reached it to act on.
	reached atomic.Bool
	// stopLeave stops the callback that ends the page's read when the
	// request's context ends; calls counts that callback and a fire while
	// they touch rc, which finish waits for.
	stopLeave func() bool
	calls     sync.WaitGroup

	mu    sync.Mutex
	timer *time.Timer
	phase phase
	// window is the current phase's bound, and moved when the timer last
	// started it: the timer fires at moved+window, and a fire before that
	// is a stale one Reset already replaced.
	window time.Duration
	moved  time.Time
	// whole is set once the page's body was read to its end, and at once
	// for an empty one.
	whole bool
	// ended is how the timer ended the carry.
	ended ending
	// reading is set while a read of the page's body waits: a body phase
	// that ends with one waiting is the page's stall, and with none the
	// upstream's, which stopped taking the body.
	reading atomic.Bool
}

// carry prepares r for the byte proxy: the request it serves, carrying
// the transfer and its cancel, and the writer to answer the page with.
// The caller ends the transfer with finish.
func (c *Carrier) carry(w http.ResponseWriter, r *http.Request) (*carriedTransfer, http.ResponseWriter, *http.Request) {
	t := &carriedTransfer{c: c, rc: http.NewResponseController(w), page: r.Context(),
		upload: r.Method != http.MethodGet && r.Method != http.MethodHead}
	// Only the timer ends a read of the page: the server's own read
	// timeout would cut a body still moving, and a read deadline passing
	// while the answer is awaited ends the request through net/http's
	// background read.
	if err := t.rc.SetReadDeadline(time.Time{}); err != nil {
		c.readDeadlineFailed(err)
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	t.cancel = cancel
	ctx = httptrace.WithClientTrace(context.WithValue(ctx, carriedKey{}, t),
		&httptrace.ClientTrace{WroteHeaders: func() { t.reached.Store(true) }})
	out := r.WithContext(ctx)
	// The proxy sends no body for a declared length of 0 (ReverseProxy),
	// which is then whole at once.
	t.phase, t.window, t.whole = phaseAwaiting, c.allowance, true
	if r.ContentLength != 0 {
		t.phase, t.window, t.whole = phaseBody, c.stall, false
		out.Body = &pageBody{ReadCloser: r.Body, t: t}
	}
	t.moved = time.Now()
	t.timer = time.AfterFunc(t.window, t.fire)
	// The page leaving, or this server shutting down, ends a read of the
	// page at once: net/http's transport waits on that read before the
	// proxy returns, so the body's Close comes too late for it.
	t.stopLeave = context.AfterFunc(r.Context(), t.leave)
	return t, transport.WriteByProgress(w, c.stall), out
}

// leave ends a read of the page when the request's context ended, unless
// the carry already finished.
func (t *carriedTransfer) leave() {
	if !t.begin() {
		return
	}
	defer t.calls.Done()
	t.abortRead()
}

// begin counts a call that touches rc, or reports false once the carry
// finished.
func (t *carriedTransfer) begin() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase == phaseDone {
		return false
	}
	t.calls.Add(1)
	return true
}

// progress restarts the window of phase p for a byte that moved in its
// direction, at most once every transport.ProgressEvery: a page byte
// restarts only the body's window, an upstream byte only the answer's.
func (t *carriedTransfer) progress(p phase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase != p {
		return
	}
	if now := time.Now(); now.Sub(t.moved) >= transport.ProgressEvery {
		t.startLocked(now, t.phase, t.window)
	}
}

// bodyEnded starts the wait for the answer at the body's last byte,
// unless the answer already began.
func (t *carriedTransfer) bodyEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.whole = true
	if t.phase == phaseBody {
		t.startLocked(time.Now(), phaseAwaiting, t.c.allowance)
	}
}

// answered starts the answer's stall window at its headers, which may
// come before the body's end (an upstream that refused it early).
func (t *carriedTransfer) answered() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase != phaseDone {
		t.startLocked(time.Now(), phaseAnswering, t.c.stall)
	}
}

func (t *carriedTransfer) startLocked(now time.Time, p phase, window time.Duration) {
	t.phase, t.window, t.moved = p, window, now
	t.timer.Reset(window)
}

// fire ends the transfer when its phase's window passed with nothing
// moving: it records how, cancels the upstream request and ends a read of
// the page.
func (t *carriedTransfer) fire() {
	t.mu.Lock()
	if t.phase == phaseDone || time.Since(t.moved) < t.window {
		t.mu.Unlock()
		return
	}
	switch {
	case t.phase == phaseBody && t.reading.Load():
		t.ended = endPageStalled
	case t.phase == phaseBody:
		t.ended = endUpstreamStalled
	case t.phase == phaseAwaiting:
		t.ended = endUnanswered
	}
	t.phase = phaseDone
	t.calls.Add(1)
	t.mu.Unlock()
	defer t.calls.Done()
	t.cancel(errTimerEnded)
	t.abortRead()
}

// failed is the page's status and sentence for a carry that ended before
// the upstream's headers, from how it ended and what was sent: whether
// the request is an upload, and whether its body went out whole.
func (t *carriedTransfer) failed() (int, string) {
	t.mu.Lock()
	ended, waited, whole := t.ended, spoken(t.window), t.whole
	t.mu.Unlock()
	// mayHave is an upload sent on whole whose answer did not come.
	mayHave := func(status int, what string) (int, string) {
		return status, "the file was sent on in full, but " + what + ", so it may have been received; check before sending it again"
	}
	const unreachable = "backend unreachable"
	switch {
	case ended == endPageStalled:
		return http.StatusRequestTimeout, "the file stopped arriving at this computer for " + waited + ", so it was not sent on in full; send it again"
	case ended == endUpstreamStalled:
		return http.StatusGatewayTimeout, "the computer that holds it stopped taking the file for " + waited + ", so it did not receive it in full; send it again"
	case ended == endUnanswered && !t.upload:
		return http.StatusGatewayTimeout, "the computer that holds it did not begin to answer within " + waited + "; try again"
	case ended == endUnanswered:
		// The answer is awaited only once the body went out whole.
		return mayHave(http.StatusGatewayTimeout, "the computer that holds it did not begin to answer within "+waited)
	case !t.upload:
		return http.StatusServiceUnavailable, unreachable
	case !t.reached.Load():
		// Before the page's context: ending the page's read after a
		// refused dial (pageBody.Close) ends that context too, and with
		// nothing gone out this is true whoever is left to read it.
		return http.StatusServiceUnavailable, "the computer that holds it could not be reached, so nothing was sent; send it again"
	case t.page.Err() != nil && !whole:
		return http.StatusServiceUnavailable, "this computer stopped before the file was sent on in full; send it again"
	case t.page.Err() != nil:
		return mayHave(http.StatusServiceUnavailable, "this computer stopped waiting for its answer")
	case !whole:
		return http.StatusBadGateway, "the connection to the computer that holds it ended before the file was sent on in full; send it again"
	}
	return mayHave(http.StatusBadGateway, "the connection to the computer that holds it ended before it answered")
}

// abortRead ends a read of the page's body in progress, and every later
// one, by moving the page's read deadline to now.
func (t *carriedTransfer) abortRead() {
	if err := t.rc.SetReadDeadline(time.Now()); err != nil {
		t.c.readDeadlineFailed(err)
	}
}

// finish ends the transfer once the carry returned. It waits for a fire
// or a leave callback that began: neither may touch the page's connection
// once the handler gave it back.
func (t *carriedTransfer) finish() {
	t.mu.Lock()
	t.phase = phaseDone
	t.timer.Stop()
	t.mu.Unlock()
	t.stopLeave()
	t.calls.Wait()
	t.cancel(nil)
}

// pageBody is the page's request body as the proxy sends it upstream.
type pageBody struct {
	io.ReadCloser
	t *carriedTransfer
}

func (b *pageBody) Read(p []byte) (int, error) {
	b.t.reading.Store(true)
	n, err := b.ReadCloser.Read(p)
	b.t.reading.Store(false)
	if n > 0 {
		b.t.progress(phaseBody)
	}
	if errors.Is(err, io.EOF) {
		b.t.bodyEnded()
	}
	return n, err
}

// Close, which the proxy defers until it returns, first ends a read of a
// body not read to its end: an upstream that answered early, or a request
// canceled (the timer, this server shutting down), leaves net/http's
// transport blocked reading it, and the close would wait behind that
// read.
func (b *pageBody) Close() error {
	b.t.mu.Lock()
	whole := b.t.whole
	b.t.mu.Unlock()
	if !whole {
		b.t.abortRead()
	}
	return b.ReadCloser.Close()
}

// upstreamBody is the upstream's answer body as the proxy copies it.
type upstreamBody struct {
	io.ReadCloser
	t *carriedTransfer
}

func (b *upstreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.t.progress(phaseAnswering)
	}
	return n, err
}

// modifyResponse is the byte proxy's ModifyResponse: the answer began.
func modifyResponse(resp *http.Response) error {
	t := resp.Request.Context().Value(carriedKey{}).(*carriedTransfer)
	t.answered()
	resp.Body = &upstreamBody{ReadCloser: resp.Body, t: t}
	return nil
}

// readDeadlineFailed reports, once per carrier, a page listener that
// cannot set a read deadline. The timer still cancels the upstream
// request at every bound; only a read of the page that is blocked waits
// for the page or the listener to end it.
func (c *Carrier) readDeadlineFailed(err error) {
	c.readDeadlineOnce.Do(func() {
		c.logf("a carried transfer cannot end a blocked read of the page (%v); the upstream is still bounded", err)
	})
}

// spoken is d as a person reads a wait.
func spoken(d time.Duration) string {
	switch {
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Round(time.Minute)/time.Minute))
	case d >= 2*time.Second:
		return fmt.Sprintf("%d seconds", int(d.Round(time.Second)/time.Second))
	}
	return fmt.Sprintf("%d milliseconds", d.Milliseconds())
}

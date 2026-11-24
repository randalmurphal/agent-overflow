package backendproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/transport"
)

// A carried transfer's timing has one rule, owned by carriedTransfer: it
// ends when no byte moves for the stall window, in either direction and in
// any phase, except that the upstream's answer has the answer allowance
// from the request's last byte to its own first byte. Nothing here is
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
//     from the upstream restarts the stall window; the page's side of it
//     is the moving write deadline (transport.WriteByProgress);
//   - done: the carry returned.
//
// One timer rather than a moving read deadline on the page: an upstream
// that stops reading the body leaves no page read pending for a deadline
// to fail, and the timer still fires.

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

// transferEnded is the cause a carried transfer's timer ends it with, and
// what the page is answered when no header was written yet.
type transferEnded struct {
	status  int
	message string
}

func (e *transferEnded) Error() string { return e.message }

// carriedTransfer is one carried request's timing.
type carriedTransfer struct {
	c      *Carrier
	rc     *http.ResponseController
	cancel context.CancelCauseFunc

	mu    sync.Mutex
	timer *time.Timer
	phase phase
	// window is the current phase's bound, and moved when the timer last
	// started it: the timer fires at moved+window, and a fire before that
	// is a stale one Reset already replaced.
	window time.Duration
	moved  time.Time
	// whole is set once the page's body was read to its end.
	whole bool
	// reading is set while a read of the page's body waits: a body phase
	// that ends with one waiting is the page's stall, and with none the
	// upstream's, which stopped taking the body.
	reading atomic.Bool
}

// carry prepares r for the byte proxy: the request it serves, carrying
// the transfer and its cancel, and the writer to answer the page with.
// The caller ends the transfer with finish.
func (c *Carrier) carry(w http.ResponseWriter, r *http.Request) (*carriedTransfer, http.ResponseWriter, *http.Request) {
	t := &carriedTransfer{c: c, rc: http.NewResponseController(w)}
	// Only the timer ends a read of the page: the server's own read
	// timeout would cut a body still moving, and a read deadline passing
	// while the answer is awaited ends the request through net/http's
	// background read.
	if err := t.rc.SetReadDeadline(time.Time{}); err != nil {
		c.readDeadlineFailed(err)
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	t.cancel = cancel
	out := r.WithContext(context.WithValue(ctx, carriedKey{}, t))
	// The proxy sends no body for a declared length of 0 (ReverseProxy).
	t.phase, t.window = phaseAwaiting, c.allowance
	if r.ContentLength != 0 {
		t.phase, t.window = phaseBody, c.stall
		out.Body = &pageBody{ReadCloser: r.Body, t: t}
	}
	t.moved = time.Now()
	t.timer = time.AfterFunc(t.window, t.fire)
	return t, transport.WriteByProgress(w, c.stall), out
}

// progress restarts the current phase's window for a byte that moved, at
// most once every transport.ProgressEvery.
func (t *carriedTransfer) progress() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase == phaseDone || t.phase == phaseAwaiting {
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
// moving: the upstream request is canceled with the phase's cause, and a
// read of the page is ended.
func (t *carriedTransfer) fire() {
	t.mu.Lock()
	if t.phase == phaseDone || time.Since(t.moved) < t.window {
		t.mu.Unlock()
		return
	}
	cause := t.endedLocked()
	t.phase = phaseDone
	t.mu.Unlock()
	t.cancel(cause)
	t.abortRead()
}

// endedLocked is the cause of a transfer that stopped in its phase,
// worded for the person at the page.
func (t *carriedTransfer) endedLocked() *transferEnded {
	switch t.phase {
	case phaseBody:
		if !t.reading.Load() {
			return &transferEnded{http.StatusGatewayTimeout,
				fmt.Sprintf("the computer that holds it stopped taking the file for %s; send it again", spoken(t.window))}
		}
		return &transferEnded{http.StatusRequestTimeout,
			fmt.Sprintf("the file stopped arriving at this computer for %s, so it was not sent on; send it again", spoken(t.window))}
	case phaseAwaiting:
		return &transferEnded{http.StatusGatewayTimeout,
			fmt.Sprintf("the computer that holds it did not begin to answer within %s; try again", spoken(t.window))}
	case phaseAnswering, phaseDone:
	}
	return &transferEnded{http.StatusGatewayTimeout,
		fmt.Sprintf("the computer that holds it stopped sending for %s; try again", spoken(t.window))}
}

// abortRead ends a read of the page's body in progress, and every later
// one, by moving the page's read deadline to now.
func (t *carriedTransfer) abortRead() {
	if err := t.rc.SetReadDeadline(time.Now()); err != nil {
		t.c.readDeadlineFailed(err)
	}
}

// finish ends the transfer once the carry returned.
func (t *carriedTransfer) finish() {
	t.mu.Lock()
	t.phase = phaseDone
	t.timer.Stop()
	t.mu.Unlock()
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
		b.t.progress()
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
		b.t.progress()
	}
	return n, err
}

// modifyResponse is the byte proxy's ModifyResponse: the answer began.
func modifyResponse(resp *http.Response) error {
	if t, ok := resp.Request.Context().Value(carriedKey{}).(*carriedTransfer); ok {
		t.answered()
		resp.Body = &upstreamBody{ReadCloser: resp.Body, t: t}
	}
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

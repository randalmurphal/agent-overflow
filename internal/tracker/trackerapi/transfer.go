package trackerapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

const (
	// stallTimeout is how long a transfer may go without a byte moving
	// while it sends or receives.
	stallTimeout = 90 * time.Second
	// headerTimeout is how long the tracker may take to answer once the
	// request is sent, which covers its processing of a large upload.
	headerTimeout = 5 * time.Minute
)

// errStalled is the cancel cause of a transfer that stopped moving.
var errStalled = errors.New("transfer stalled")

// stall bounds one attempt of a transfer: it cancels the attempt's
// context when no byte moves for stallTimeout while sending or receiving,
// or when the answer's headers take longer than headerTimeout after the
// request is sent. Its methods are no-ops on nil, the stall of a call
// that is not a transfer.
type stall struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	timer   *time.Timer
	waiting atomic.Bool // the request is sent and no answer has come
}

// newStall starts timing an attempt. sending says whether a body goes
// first; without one the wait for the answer starts at once.
func newStall(parent context.Context, sending bool) *stall {
	ctx, cancel := context.WithCancelCause(parent)
	s := &stall{ctx: ctx, cancel: cancel}
	s.waiting.Store(!sending)
	first := stallTimeout
	if !sending {
		first = headerTimeout
	}
	s.timer = time.AfterFunc(first, func() {
		if s.waiting.Load() {
			cancel(fmt.Errorf("%w: no answer within %s of sending", errStalled, headerTimeout))
			return
		}
		cancel(fmt.Errorf("%w: no data moved for %s", errStalled, stallTimeout))
	})
	return s
}

func (s *stall) context(parent context.Context) context.Context {
	if s == nil {
		return parent
	}
	return s.ctx
}

func (s *stall) stop() {
	if s == nil {
		return
	}
	s.timer.Stop()
	s.cancel(nil)
}

// sent starts the wait for the answer once the request body is out.
func (s *stall) sent() {
	s.waiting.Store(true)
	s.timer.Reset(headerTimeout)
}

// receiving starts the progress timer once the answer's headers arrived.
func (s *stall) receiving() {
	if s == nil {
		return
	}
	s.waiting.Store(false)
	s.timer.Reset(stallTimeout)
}

func (s *stall) moved() { s.timer.Reset(stallTimeout) }

// explain replaces the cancellation a stall caused with its cause.
func (s *stall) explain(err error) error {
	if s == nil {
		return err
	}
	if cause := context.Cause(s.ctx); errors.Is(cause, errStalled) {
		return cause
	}
	return err
}

// reader counts an answer body's progress.
func (s *stall) reader(r io.Reader) io.Reader {
	if s == nil {
		return r
	}
	return &progressReader{r: r, s: s, left: -1}
}

// upload is a request body that counts its progress and starts the wait
// for the answer once it is all read, with the GetBody a redirect that
// resends it uses.
func (s *stall) upload(body []byte) (io.ReadCloser, func() (io.ReadCloser, error)) {
	open := func() (io.ReadCloser, error) {
		return io.NopCloser(&progressReader{r: bytes.NewReader(body), s: s, left: int64(len(body))}), nil
	}
	first, _ := open()
	return first, open
}

type progressReader struct {
	r    io.Reader
	s    *stall
	left int64 // upload bytes not yet read; below zero for an answer body
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.s.moved()
	}
	if p.left >= 0 {
		if n > 0 {
			// A resend after a redirect is sending again.
			p.s.waiting.Store(false)
		}
		p.left -= int64(n)
		if p.left <= 0 || err != nil {
			p.left = -1
			p.s.sent()
		}
	}
	return n, err
}

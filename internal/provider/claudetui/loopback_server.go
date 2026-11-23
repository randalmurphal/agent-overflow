package claudetui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// stopGrace is how long stop lets active requests finish before it cancels
// them and closes their connections.
const stopGrace = 2 * time.Second

// loopbackServer is the serving lifecycle the gateway and the hook relay
// share. Its contract is that no handler runs once stop returns: a handler
// counts itself in before it touches the session, stop refuses new ones and
// waits for the counted ones, and the serve goroutine has returned.
type loopbackServer struct {
	name    string
	ln      net.Listener
	server  *http.Server
	onError func(error)
	// cancel ends the context every request's context derives from.
	cancel    context.CancelFunc
	serveDone chan struct{}

	mu      sync.Mutex
	idle    *sync.Cond
	closing bool
	active  int
}

// newLoopbackServer serves handler on ln once started. configure may set
// server options beyond the handler and the base context.
func newLoopbackServer(name string, ln net.Listener, handler http.HandlerFunc, onError func(error), configure func(*http.Server)) *loopbackServer {
	ctx, cancel := context.WithCancel(context.Background())
	l := &loopbackServer{name: name, ln: ln, onError: onError, cancel: cancel}
	l.idle = sync.NewCond(&l.mu)
	l.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.enter() {
				http.Error(w, "closing", http.StatusServiceUnavailable)
				return
			}
			defer l.leave()
			handler(w, r)
		}),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	if configure != nil {
		configure(l.server)
	}
	return l
}

// url is the server's loopback base URL.
func (l *loopbackServer) url() string {
	return "http://" + l.ln.Addr().String()
}

// start serves until stop. A serve failure other than the stop is surfaced.
func (l *loopbackServer) start() {
	l.serveDone = make(chan struct{})
	go func() {
		defer close(l.serveDone)
		if err := l.server.Serve(l.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.onError(fmt.Errorf("%s serve: %w", l.name, err))
		}
	}()
}

func (l *loopbackServer) enter() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return false
	}
	l.active++
	return true
}

func (l *loopbackServer) leave() {
	l.mu.Lock()
	l.active--
	if l.active == 0 {
		l.idle.Broadcast()
	}
	l.mu.Unlock()
}

// stop closes the listener and lets active requests finish for stopGrace.
// Past it, it cancels their contexts and closes their connections, and
// reports that once. It returns after every handler that entered has
// returned and the serve goroutine has ended, so nothing it served can touch
// the session afterwards. Safe to call once.
func (l *loopbackServer) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), stopGrace)
	defer cancel()
	err := l.server.Shutdown(ctx)
	if err != nil {
		l.cancel()
		err = fmt.Errorf("%s: requests still active after %s were cut off: %w", l.name, stopGrace, errors.Join(err, l.server.Close()))
	}
	l.mu.Lock()
	l.closing = true
	for l.active > 0 {
		l.idle.Wait()
	}
	l.mu.Unlock()
	l.cancel()
	if l.serveDone != nil {
		<-l.serveDone
	}
	return err
}

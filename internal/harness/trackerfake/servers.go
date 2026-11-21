package trackerfake

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// Servers are an engine's origins on loopback listeners: one per Jira
// site named, one for Tempo and one for Linear. Each is an http.Server
// Servers owns, served by one goroutine that Close ends. Close stops
// them; it does not close the engine.
type Servers struct {
	jira   map[string]*server
	tempo  *server
	linear *server

	closeOnce sync.Once
	// serving counts the serve goroutines; live is how many have not
	// returned.
	serving sync.WaitGroup
	live    atomic.Int32

	// mu guards closed and the Add of answering, so no request starts
	// counting once Close waits.
	mu        sync.Mutex
	closed    bool
	answering sync.WaitGroup
	// err is the first error a server stopped with other than Close.
	err error
}

// server is one origin's listener and http.Server, and the connections
// open to it.
type server struct {
	origin Origin
	url    string
	srv    *http.Server

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// Serve starts e's origins on 127.0.0.1 listeners: the Tempo and Linear
// APIs and one server per Jira site in sites. A failure names the origin
// and closes every server already started.
func Serve(e *Engine, sites ...string) (*Servers, error) {
	return serve(e, net.Listen, sites)
}

func serve(e *Engine, listen func(network, addr string) (net.Listener, error), sites []string) (*Servers, error) {
	s := &Servers{jira: map[string]*server{}}
	origins := make([]Origin, 0, len(sites)+2)
	for _, site := range sites {
		if containsSite(origins, site) {
			return nil, fmt.Errorf("trackerfake: serve: site %s named twice", site)
		}
		origins = append(origins, Origin{Tracker: TrackerJira, Site: site})
	}
	origins = append(origins, Origin{Tracker: TrackerTempo}, Origin{Tracker: TrackerLinear})
	for _, o := range origins {
		srv, err := s.start(e, listen, o)
		if err != nil {
			s.Close()
			return nil, err
		}
		switch o.Tracker {
		case TrackerJira:
			s.jira[o.Site] = srv
		case TrackerTempo:
			s.tempo = srv
		case TrackerLinear:
			s.linear = srv
		}
	}
	return s, nil
}

func containsSite(origins []Origin, site string) bool {
	for _, o := range origins {
		if o.Site == site {
			return true
		}
	}
	return false
}

// start listens for o and serves it on a goroutine s owns.
func (s *Servers) start(e *Engine, listen func(network, addr string) (net.Listener, error), o Origin) (*server, error) {
	h, err := e.Handler(o)
	if err != nil {
		return nil, err
	}
	ln, err := listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("trackerfake: serve %s: listen: %w", o, err)
	}
	srv := &server{origin: o, url: "http://" + ln.Addr().String(), conns: map[net.Conn]struct{}{}}
	srv.srv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.begin() {
				panic(http.ErrAbortHandler)
			}
			defer s.answering.Done()
			h.ServeHTTP(w, r)
		}),
		ConnState: srv.track,
	}
	s.serving.Add(1)
	s.live.Add(1)
	go func() {
		defer s.serving.Done()
		defer s.live.Add(-1)
		// Serve returns http.ErrServerClosed once Close shuts the server;
		// any other end is kept for Err.
		if err := srv.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			s.fail(fmt.Errorf("trackerfake: serve %s: %w", o, err))
		}
	}()
	return srv, nil
}

// begin counts a request being answered; false once Close has begun.
func (s *Servers) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.answering.Add(1)
	return true
}

func (s *Servers) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// Err is the first error a server stopped serving with before Close;
// nil while every server serves.
func (s *Servers) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// track keeps the server's open connections for CloseClientConnections.
func (srv *server) track(c net.Conn, state http.ConnState) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	switch state {
	case http.StateNew:
		srv.conns[c] = struct{}{}
	case http.StateClosed, http.StateHijacked:
		delete(srv.conns, c)
	case http.StateActive, http.StateIdle:
		// Still open, and tracked since StateNew.
	}
}

// JiraURL is the base URL of site's server; empty when Serve did not
// start one for it.
func (s *Servers) JiraURL(site string) string {
	if srv := s.jira[site]; srv != nil {
		return srv.url
	}
	return ""
}

// TempoURL is the Tempo API's base URL.
func (s *Servers) TempoURL() string { return s.tempo.url }

// LinearURL is the Linear API's origin.
func (s *Servers) LinearURL() string { return s.linear.url }

// CloseClientConnections closes every open connection to the servers,
// so a client's next request opens a new one. A request being answered
// on a closed connection ends with its client's context.
func (s *Servers) CloseClientConnections() {
	for _, srv := range s.all() {
		srv.mu.Lock()
		for c := range srv.conns {
			// A connection already closed by its peer has nothing to
			// report; the server drops it on its own.
			_ = c.Close()
		}
		srv.mu.Unlock()
	}
}

// Close shuts every server: their listeners and connections close, and
// Close returns once each serve goroutine has returned and every request
// being answered has ended (a held one ends with its closed connection).
// Close may be called more than once; requests after it fail to connect.
func (s *Servers) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		for _, srv := range s.all() {
			// Close reports only the listener's own close error, which
			// leaves nothing to do: the server is shut either way.
			_ = srv.srv.Close()
		}
		s.serving.Wait()
		s.answering.Wait()
	})
}

func (s *Servers) all() []*server {
	out := sortedValues(s.jira)
	for _, srv := range []*server{s.tempo, s.linear} {
		if srv != nil {
			out = append(out, srv)
		}
	}
	return out
}

package app

import "sync"

// peerAdmissions is the tokens of the request-minting calls (spawn, send,
// ask) a paired computer has a handler in flight for here, from the
// handler's entry to its exit. Only a handler in this set can still admit
// its token, so a status answer for a token with no receipt says whether
// one may (ThreadPeerRequest.Admitting): with no handler in flight, the
// token was never started here and nothing running here will start it.
//
// Memory only: a restart ends every handler, and empties the set with them.
type peerAdmissions struct {
	mu sync.Mutex
	// tokens counts the handlers in flight for each token: a retry can
	// arrive while the attempt before it is still running.
	tokens map[string]int
}

// enter records one handler for token and returns its exit, which the
// handler runs once it can no longer admit the token.
func (p *peerAdmissions) enter(token string) (exit func()) {
	p.mu.Lock()
	if p.tokens == nil {
		p.tokens = make(map[string]int)
	}
	p.tokens[token]++
	p.mu.Unlock()
	return sync.OnceFunc(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.tokens[token]--; p.tokens[token] <= 0 {
			delete(p.tokens, token)
		}
	})
}

// holds reports whether a handler for token is in flight.
func (p *peerAdmissions) holds(token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokens[token] > 0
}

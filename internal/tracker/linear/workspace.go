package linear

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"agent-overflow/internal/tracker/trackerapi"
)

// A client bound to a workspace (Options.Workspace) reads its key through
// a boundSource. Each time the transport reads the key (at first use,
// after ForgetCredential, and after a 401), the source asks Linear, with
// the key just read, which workspace it belongs to, and hands the key to
// the transport only when that is the client's. The check runs inside the
// transport's credential read, so no request, a write resent after a 401
// included, ever carries a key that was not checked; concurrent calls
// share the one read and its check, and a key costs one check, not one per
// call. A transport CredentialMark is known only once an answer arrives,
// too late to hold back the request it marks, so the check keys on the
// reads themselves.

// WrongWorkspaceError is a key that belongs to another workspace than the
// one the client is for. A call that met it was not sent: its error is a
// *trackerapi.NotSentError holding this one. Viewer, which reads the
// workspace itself, returns it bare.
type WrongWorkspaceError struct {
	Want string // the URL key the client is for
	Got  string // the URL key of the key's workspace
}

func (e *WrongWorkspaceError) Error() string {
	return fmt.Sprintf("linear: the key belongs to workspace %q, not %q", e.Got, e.Want)
}

// sameWorkspace compares URL keys, which Linear treats without case.
func sameWorkspace(a, b string) bool { return strings.EqualFold(a, b) }

// boundSource is the credential source of a client bound to want. A key
// found to be another workspace's is refused, without reading the source
// or sending anything, until forget: a key is replaced only by a read
// after ForgetCredential or a 401, and a refused key sends nothing that
// could be answered 401. A check that fails for another reason fails the
// read, which the transport tries again on the next call.
type boundSource struct {
	want  string
	inner trackerapi.CredentialSource
	// checker is an unbound client whose transport reads candidate, the
	// key under check.
	checker   *Client
	candidate *candidate

	mu    sync.Mutex
	epoch uint64 // advanced by forget
	wrong *WrongWorkspaceError
}

// Credential reads the key and returns it once Linear has placed it in
// want. The transport calls it from one goroutine at a time.
func (s *boundSource) Credential(ctx context.Context) (*trackerapi.Credential, error) {
	s.mu.Lock()
	wrong, epoch := s.wrong, s.epoch
	s.mu.Unlock()
	if wrong != nil {
		return nil, wrong
	}
	cred, err := s.inner.Credential(ctx)
	if err != nil {
		return cred, err
	}
	v, err := s.ask(ctx, cred)
	if err != nil {
		cred.Zero()
		return nil, fmt.Errorf("linear: check which workspace the key belongs to: %w", err)
	}
	if !sameWorkspace(v.Organization.URLKey, s.want) {
		cred.Zero()
		wrong := &WrongWorkspaceError{Want: s.want, Got: v.Organization.URLKey}
		s.mu.Lock()
		if s.epoch == epoch {
			s.wrong = wrong
		}
		s.mu.Unlock()
		return nil, wrong
	}
	return cred, nil
}

// ask reads the viewer with cred alone, and leaves the checker holding
// no key.
func (s *boundSource) ask(ctx context.Context, cred *trackerapi.Credential) (*Viewer, error) {
	s.candidate.set(trackerapi.StaticCredentials(cred))
	s.checker.http.ForgetCredential()
	defer func() {
		s.candidate.set(nil)
		s.checker.http.ForgetCredential()
	}()
	return s.checker.Viewer(ctx)
}

// forget clears a refusal, so the next read is checked again; a check
// in flight when it runs does not refuse its key for later reads.
func (s *boundSource) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	s.wrong = nil
}

// candidate is the checker's credential source: the key under check.
type candidate struct {
	mu  sync.Mutex
	src trackerapi.CredentialSource
}

func (c *candidate) set(src trackerapi.CredentialSource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.src = src
}

func (c *candidate) Credential(ctx context.Context) (*trackerapi.Credential, error) {
	c.mu.Lock()
	src := c.src
	c.mu.Unlock()
	if src == nil {
		return nil, errors.New("linear: no key is under check")
	}
	return src.Credential(ctx)
}

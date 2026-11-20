package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/trackerapi"
)

// verifyBound is how long one verification's identity call may take. The
// transport's own read bound (45 seconds) is longer.
const verifyBound = 20 * time.Second

// Verify drops connection id's held credential, reads its key file again
// and makes one identity call to the tracker: who the token belongs to
// for Jira and Linear, and Tempo's settings, which need a valid token and
// name nobody. A Linear key for another workspace than the connection's
// is refused with the workspace it belongs to.
//
// Success records the verification and clears the connection's problem.
// A failed call is an outcome, not an error: the connection is returned
// with its problem set. The error is for an unknown connection
// (ErrNotFound), one turned off (ErrDisabled), a store failure, and a
// call Stop overtook (ErrStopped). A connection turned off or removed
// while the call ran records nothing.
func (s *Service) Verify(ctx context.Context, id string) (Connection, error) {
	if err := s.enter(); err != nil {
		return Connection{}, err
	}
	defer s.leave()

	s.opMu.Lock()
	row, err := s.get(id)
	if err != nil {
		s.opMu.Unlock()
		return Connection{}, err
	}
	if !row.Enabled {
		s.opMu.Unlock()
		return Connection{}, fmt.Errorf("%w: %s", ErrDisabled, id)
	}
	c, err := s.use(row)
	if err != nil {
		defer s.opMu.Unlock()
		if _, ok := errors.AsType[*ClientError](err); !ok {
			return Connection{}, err
		}
		// No client means no call: the connection's problem is why.
		s.setProblem(id, ClassifyFailure(row.Kind, err))
		s.changed()
		return s.view(row), nil
	}
	s.opMu.Unlock()

	verification, callErr := s.identity(ctx, row, c)

	s.opMu.Lock()
	out, moved, err := s.settleVerification(row, c, verification, callErr)
	s.opMu.Unlock()
	if moved {
		s.changed()
	}
	return out, err
}

// identity is the verification's one tracker call, under verifyBound and
// the service's lifetime.
func (s *Service) identity(ctx context.Context, row store.TrackerConnection, c *clients) (store.TrackerVerification, error) {
	// A person waits on a verification, so it is interactive: a rate
	// limit that lifts within the transport's short wait is waited out.
	ctx, cancel := context.WithTimeout(trackerapi.WithInteractive(ctx), verifyBound)
	defer cancel()
	stop := context.AfterFunc(s.life, cancel)
	defer stop()
	c.forgetCredential()
	switch row.Kind {
	case store.TrackerKindJira:
		p, err := jiraprovider.New(jiraprovider.Options{Jira: c.jira, Site: row.Site})
		if err != nil {
			return store.TrackerVerification{}, err
		}
		u, err := p.Identity(ctx)
		if err != nil {
			return store.TrackerVerification{}, err
		}
		return store.TrackerVerification{AccountID: string(u.ID), Name: u.Name}, nil
	case store.TrackerKindTempo:
		// Settings are cached; dropping them makes the read a request.
		c.tempo.DropCache()
		if _, err := c.tempo.Settings(ctx); err != nil {
			return store.TrackerVerification{}, err
		}
		return store.TrackerVerification{}, nil
	}
	v, err := c.linear.Viewer(ctx)
	if err != nil {
		return store.TrackerVerification{}, err
	}
	if got := strings.ToLower(v.Organization.URLKey); got != row.Site {
		wrong := &WrongWorkspaceError{Want: row.Site}
		if linearURLKey.MatchString(got) {
			wrong.Got = got
		}
		return store.TrackerVerification{}, wrong
	}
	name := v.User.Name
	if name == "" {
		name = v.User.DisplayName
	}
	return store.TrackerVerification{AccountID: v.User.ID, Name: name}, nil
}

// settleVerification records the outcome of a verification of row made
// with c, when row is still in use with c. The caller holds opMu.
func (s *Service) settleVerification(row store.TrackerConnection, c *clients, v store.TrackerVerification, callErr error) (Connection, bool, error) {
	if !s.inUse(row.ID, c) {
		if s.isStopped() {
			return Connection{}, false, ErrStopped
		}
		current, err := s.get(row.ID)
		if err != nil {
			return Connection{}, false, err
		}
		return s.view(current), false, nil
	}
	if callErr != nil {
		s.setProblem(row.ID, ClassifyFailure(row.Kind, callErr))
		return s.view(row), true, nil
	}
	v.VerifiedAt = s.now().UnixMilli()
	recorded, _, err := s.store.RecordTrackerConnectionVerification(row.ID, v)
	if err != nil {
		return Connection{}, false, s.writeError(row.ID, "record the verification of", err)
	}
	s.clearProblem(row.ID)
	return s.view(recorded), true, nil
}

// setProblem holds f as connection id's last failure, unless the service
// stopped.
func (s *Service) setProblem(id string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.problems[id] = f
	}
}

func (s *Service) clearProblem(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.problems, id)
}

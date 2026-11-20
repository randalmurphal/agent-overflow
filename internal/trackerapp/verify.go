package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/trackerapi"
)

// verifyBound is how long one verification's identity call may take. The
// transport's own read bound (45 seconds) is longer.
const verifyBound = 20 * time.Second

// Verify reads connection id's key file, so its client uses a token
// replaced there, and makes one identity call to the tracker: who the
// token belongs to for Jira and Linear, and Tempo's settings, which need
// a valid token and name nobody. The Linear client refuses a key for
// another workspace than the connection's, naming the one it belongs to.
//
// Success records the verification and clears the connection's problem.
// A failed call is an outcome, not an error: the connection is returned
// with its problem set (settle). The error is for an unknown connection
// (ErrNotFound), one turned off (ErrDisabled), a store failure, and a
// call Stop overtook (ErrStopped). A verification records nothing when
// its connection was turned off or removed while it ran, or when a
// verification begun after it already recorded its outcome.
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
		moved, err := s.settle(id, row.Kind, stamp{}, err, nil)
		if err != nil {
			return Connection{}, err
		}
		if moved {
			s.changed()
		}
		return s.view(row), nil
	}
	at := s.begin(c)
	s.opMu.Unlock()

	// The key file read that shows its state also hands a replaced token
	// to the client, so the call checks the token in the file.
	file := s.look(row).KeyFile
	verification, callErr := s.identity(ctx, row, c)

	s.opMu.Lock()
	out, moved, err := s.settleVerification(row, at, file, verification, callErr)
	s.forgetHolds()
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
	// The client refuses a key of another workspace than row.Site.
	v, err := c.linear.Viewer(ctx)
	if err != nil {
		return store.TrackerVerification{}, err
	}
	name := v.User.Name
	if name == "" {
		name = v.User.DisplayName
	}
	return store.TrackerVerification{AccountID: v.User.ID, Name: name}, nil
}

// settleVerification records the outcome of the verification of row that
// began at at, through settle: one whose outcome settle does not apply
// records nothing, its verification row included, and returns the
// connection as it is. file is the key file's state the verification
// read. The caller holds opMu.
func (s *Service) settleVerification(row store.TrackerConnection, at stamp, file keyfile.Status, v store.TrackerVerification, callErr error) (Connection, bool, error) {
	record := func() error {
		v.VerifiedAt = s.now().UnixMilli()
		recorded, _, err := s.store.RecordTrackerConnectionVerification(row.ID, v)
		if err != nil {
			return s.writeError(row.ID, "record the verification of", err)
		}
		row = recorded
		return nil
	}
	moved, err := s.settle(row.ID, row.Kind, at, callErr, record)
	switch {
	case err != nil:
		return Connection{}, false, err
	case moved:
		return s.viewWith(row, file), true, nil
	}
	current, err := s.get(row.ID)
	if err != nil {
		return Connection{}, false, err
	}
	return s.view(current), false, nil
}

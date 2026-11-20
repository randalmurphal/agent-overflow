package trackerapp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/linearprovider"
)

// Store is the tracker connection accessors the service uses.
type Store interface {
	CreateTrackerConnection(c store.NewTrackerConnection) (store.TrackerConnection, error)
	GetTrackerConnection(id string) (store.TrackerConnection, error)
	ListTrackerConnections() ([]store.TrackerConnection, error)
	SetTrackerConnectionLabel(id, label string) (store.TrackerConnection, bool, error)
	SetTrackerConnectionEnabled(id string, enabled bool) (store.TrackerConnection, bool, error)
	SetTrackerConnectionTokenExpiry(id string, expiresAt int64) (store.TrackerConnection, bool, error)
	RecordTrackerConnectionVerification(id string, v store.TrackerVerification) (store.TrackerConnection, bool, error)
	DeleteTrackerConnection(id string) ([]string, error)
}

// Deps are the service's capabilities. Every field is required.
type Deps struct {
	Store Store
	// Changed is told after every change a list of connections would show
	// differently: a row, a key file the service created or removed, a
	// verification, a problem. It must not block.
	Changed func()
	// CredentialsDir is the owner-only folder the key files live in
	// (keyfile.DirName under the app's config folder).
	CredentialsDir string
	// Origin says where a connection's client connects.
	Origin Origin
	// Version is the app version the clients send in their User-Agent.
	Version string
	// Now is the clock verification times are read from.
	Now func() time.Time
	// Life ends the calls the service makes when the app stops.
	Life context.Context
}

var (
	// ErrStopped is a call after Stop, or one Stop overtook.
	ErrStopped = errors.New("trackerapp: the tracker service has stopped")
	// ErrNotFound is a connection that does not exist. It also matches
	// sql.ErrNoRows.
	ErrNotFound = errors.New("trackerapp: no such tracker connection")
	// ErrDisabled is a connection that is turned off.
	ErrDisabled = errors.New("trackerapp: the tracker connection is turned off")
	// ErrNoProvider is a Tempo connection asked for a provider: its
	// worklogs are part of its Jira connection's.
	ErrNoProvider = errors.New("trackerapp: a Tempo connection is used through its Jira connection")
)

// Service owns the tracker connections of this computer: their rows'
// changes, their key files, their clients and the last problem each one
// had. Make it with New; Stop releases it.
type Service struct {
	store          Store
	changed        func()
	credentialsDir string
	origin         Origin
	version        string
	now            func() time.Time
	life           context.Context
	// digestKey keys the token digests the credential sources keep. It is
	// random per service and never leaves it.
	digestKey []byte

	// opMu serializes every change to a connection, from its first store
	// read to its last write. It is never held across a tracker call.
	opMu sync.Mutex

	// mu guards the registry below. It is held only for map and flag
	// operations, never across a store call, a file or a tracker call.
	mu sync.Mutex
	// clients holds the client of each connection in use. A connection
	// enters on its first use (use) and leaves when it stops being used
	// (stopUsing); a caller holding an entry compares it by identity to
	// learn whether the connection stayed in use meanwhile.
	clients map[string]*clients
	// problems holds the last failure of each connection turned on; a
	// connection that stops being used forgets its own.
	problems map[string]Failure
	stopped  bool
	inflight sync.WaitGroup
}

// New makes the service. It refuses a missing dependency.
func New(deps Deps) (*Service, error) {
	switch {
	case deps.Store == nil, deps.Changed == nil, deps.Origin == nil, deps.Now == nil, deps.Life == nil:
		return nil, errors.New("trackerapp: Deps needs Store, Changed, Origin, Now and Life")
	case deps.CredentialsDir == "":
		return nil, errors.New("trackerapp: Deps.CredentialsDir is required")
	case deps.Version == "":
		return nil, errors.New("trackerapp: Deps.Version is required for the clients' User-Agent")
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key) // crypto/rand.Read never fails; it crashes instead
	return &Service{
		store: deps.Store, changed: deps.Changed, credentialsDir: deps.CredentialsDir,
		origin: deps.Origin, version: deps.Version, now: deps.Now, life: deps.Life,
		digestKey: key,
		clients:   map[string]*clients{}, problems: map[string]Failure{},
	}, nil
}

// Stop closes every client, which ends the tracker calls in flight, and
// waits for the service's calls in flight to return. Every later call
// fails with ErrStopped. Stop may be called more than once.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	open := s.clients
	s.clients = map[string]*clients{}
	clear(s.problems)
	s.mu.Unlock()
	for _, c := range open {
		c.close()
	}
	s.inflight.Wait()
}

// enter admits one call, or refuses it after Stop; leave ends it.
func (s *Service) enter() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrStopped
	}
	s.inflight.Add(1)
	return nil
}

func (s *Service) leave() { s.inflight.Done() }

// use is where a connection starts being used: it returns the client of
// row, building it on first use. The caller holds opMu and has read row
// under it, so a connection turned off since cannot gain a client.
func (s *Service) use(row store.TrackerConnection) (*clients, error) {
	if !row.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrDisabled, row.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, ErrStopped
	}
	if c := s.clients[row.ID]; c != nil {
		return c, nil
	}
	c, err := s.buildClients(row)
	if err != nil {
		return nil, err
	}
	s.clients[row.ID] = c
	return c, nil
}

// stopUsing is where a connection stops being used: its client is closed,
// which ends its calls in flight, and its problem is forgotten. A Jira
// connection takes the Tempo connection that belongs to it along, since
// a provider made while both were in use holds both clients; the Tempo
// row is left as it is, and its next use builds a new client. It reports
// whether a problem was forgotten. The caller holds opMu.
func (s *Service) stopUsing(id string) bool {
	s.mu.Lock()
	_, had := s.problems[id]
	delete(s.problems, id)
	var closing []*clients
	for cid, c := range s.clients {
		if cid == id || c.parentID == id {
			closing = append(closing, c)
			delete(s.clients, cid)
			if _, ok := s.problems[cid]; ok {
				had = true
				delete(s.problems, cid)
			}
		}
	}
	s.mu.Unlock()
	for _, c := range closing {
		c.close()
	}
	return had
}

func (s *Service) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// get reads connection id, refusing a missing one with ErrNotFound.
func (s *Service) get(id string) (store.TrackerConnection, error) {
	row, err := s.store.GetTrackerConnection(id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.TrackerConnection{}, fmt.Errorf("%w %s: %w", ErrNotFound, id, err)
	}
	if err != nil {
		return store.TrackerConnection{}, fmt.Errorf("trackerapp: read connection %s: %w", id, err)
	}
	return row, nil
}

// Connection is a connection as the service shows it: the row, where its
// key file is and the file's state, and its last problem. Never the
// token.
type Connection struct {
	store.TrackerConnection
	KeyFilePath string
	KeyFile     keyfile.Status
	// Problem is the connection's last failure; nil when its last call
	// succeeded, or none failed since it was last turned on.
	Problem *Failure
}

// view is row as Connection, its key file inspected now.
func (s *Service) view(row store.TrackerConnection) Connection {
	return s.viewWith(row, s.keyFile(row).Inspect())
}

// viewWith is row as Connection with its key file in state file.
func (s *Service) viewWith(row store.TrackerConnection, file keyfile.Status) Connection {
	v := Connection{TrackerConnection: row, KeyFilePath: s.keyFilePath(row.ID), KeyFile: file}
	s.mu.Lock()
	if p, ok := s.problems[row.ID]; ok {
		v.Problem = &p
	}
	s.mu.Unlock()
	return v
}

// look is row as Connection from one read of its key file, which also
// tells the connection's client, when it is in use, whether the token in
// the file is still the one it was handed (clients.syncCredential).
func (s *Service) look(row store.TrackerConnection) Connection {
	token, err := s.keyFile(row).Read()
	s.mu.Lock()
	c := s.clients[row.ID]
	s.mu.Unlock()
	if c != nil {
		c.syncCredential(token.Reveal())
	}
	token.Zero()
	return s.viewWith(row, keyfile.StatusOf(err))
}

// Connections lists every connection in the order they were made. A
// client whose token was replaced in its key file reads the file again on
// its next call; no other client is touched.
func (s *Service) Connections() ([]Connection, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	rows, err := s.store.ListTrackerConnections()
	if err != nil {
		return nil, fmt.Errorf("trackerapp: list connections: %w", err)
	}
	out := make([]Connection, len(rows))
	for i, row := range rows {
		out[i] = s.look(row)
	}
	return out, nil
}

// stamp is when a call on a connection began: the client it was made with
// and its place among the calls begun on that client, which orders the
// call's outcome against theirs (settle). The zero stamp is a failure to
// start using the connection at all.
type stamp struct {
	c   *clients
	gen uint64
}

// begin stamps a call about to be made with c, the client of a
// connection in use.
func (s *Service) begin(c *clients) stamp {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.gen++
	return stamp{c: c, gen: c.gen}
}

// settle is the one place a connection's problem is set or cleared: the
// outcome of the call on connection id, of kind, that began at at.
// callErr nil is a success, which clears the problem; record, when not
// nil, runs first and its error stops the settlement (a verification's
// row write). A failure becomes the problem as ClassifyFailure sorts it.
//
// An outcome is applied unless its client is no longer the connection's
// (the connection stopped being used, or Stop) or the outcome of a call
// begun later on the same client was already applied. Otherwise settle
// changes nothing and reports false, so an older outcome never replaces a
// newer one, while calls that overlap still each apply theirs in turn.
// A zero at applies only while the connection has no client. The caller
// holds opMu, which serializes settlements and keeps the connection in
// use during record.
func (s *Service) settle(id, kind string, at stamp, callErr error, record func() error) (bool, error) {
	s.mu.Lock()
	// Stop empties clients, so a call Stop overtook finds no client.
	current := s.clients[id] == at.c && (at.c == nil || at.gen > at.c.settled)
	s.mu.Unlock()
	if !current {
		return false, nil
	}
	if callErr == nil && record != nil {
		if err := record(); err != nil {
			return false, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.c != nil {
		at.c.settled = at.gen
	}
	if callErr != nil {
		s.problems[id] = ClassifyFailure(kind, callErr)
	} else {
		delete(s.problems, id)
	}
	return true, nil
}

// Provider is the tracker of connection id, for a Jira or Linear
// connection that is turned on. A Jira connection's provider logs time
// through its Tempo connection when that one is turned on, and through
// Jira's own worklogs otherwise, as the connections are at this call. It
// fails for an unknown connection, one turned off and a Tempo connection.
// The provider stays usable until its connection, or its Tempo
// connection, stops being used; then its calls fail.
func (s *Service) Provider(ctx context.Context, id string) (tracker.Provider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	row, err := s.get(id)
	if err != nil {
		return nil, err
	}
	if row.Kind == store.TrackerKindTempo {
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, id)
	}
	c, err := s.use(row)
	if err != nil {
		return nil, err
	}
	if row.Kind == store.TrackerKindLinear {
		return linearprovider.New(c.linear)
	}
	opts := jiraprovider.Options{Jira: c.jira, Site: row.Site}
	timeRow, ok, err := s.tempoOf(id)
	if err != nil {
		return nil, err
	}
	if ok && timeRow.Enabled {
		t, err := s.use(timeRow)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: the Tempo connection of %s: %w", id, err)
		}
		opts.Tempo = t.tempo
	}
	return jiraprovider.New(opts)
}

// tempoOf reads the Tempo connection that belongs to Jira connection id.
func (s *Service) tempoOf(id string) (store.TrackerConnection, bool, error) {
	rows, err := s.store.ListTrackerConnections()
	if err != nil {
		return store.TrackerConnection{}, false, fmt.Errorf("trackerapp: list connections: %w", err)
	}
	for _, row := range rows {
		if row.ParentID == id {
			return row, true, nil
		}
	}
	return store.TrackerConnection{}, false, nil
}

// jiraClient is the client of Jira connection id, for its Tempo
// connection's issue lookups.
func (s *Service) jiraClient(id string) (*jira.Client, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	row, err := s.get(id)
	if err != nil {
		return nil, err
	}
	c, err := s.use(row)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: the Jira connection %s: %w", id, err)
	}
	return c.jira, nil
}

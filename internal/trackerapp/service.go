package trackerapp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/linearprovider"
)

// Store is the tracker connection and mirror accessors the service uses.
type Store interface {
	MirrorStore
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
	// holds keeps, by connection, what a tick reuses so that it reads
	// nothing but its scope's sync state: the row, the mirror root and the
	// provider (unit.go). Every change to a connection made through the
	// service drops them all (forgetHolds), as does Stop. Guarded by opMu.
	holds map[string]*connHold

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
	// locks are the scope and history locks of the ticks in flight
	// (tick.go); unexplained the references each scope's last structure
	// read left unexplained (unexplained.go), unexplainedUse their write
	// counter.
	locks          map[lockKey]*scopeLock
	unexplained    map[Scope]*unexplainedSet
	unexplainedUse uint64

	// logf records what fails after the operation that caused it has
	// succeeded: a mirror's drain. Tests replace it.
	logf func(format string, args ...any)

	// beforePublish, set only by tests, runs in settle between its record
	// and the publication, where Stop may overtake a settlement.
	beforePublish func()
	// makeProvider builds the provider of a connection in use from its
	// client; tests replace it with a scripted provider.
	makeProvider func(row store.TrackerConnection, c *clients, tempo *clients) (tracker.Provider, error)
}

// New makes the service. It refuses a missing dependency, and reads,
// builds and starts nothing else: a client is built on a connection's
// first use, and the drain of mirrors a turn off or removal left is the
// app's call to FinishMirrorCleanup.
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
		locks: map[lockKey]*scopeLock{}, unexplained: map[Scope]*unexplainedSet{}, holds: map[string]*connHold{},
		makeProvider: buildProvider,
		logf:         log.Printf,
	}, nil
}

// Stop closes every client, which ends the tracker calls in flight, and
// waits for the service's calls in flight to return. Every later call
// fails with ErrStopped. Stop may be called more than once.
func (s *Service) Stop() {
	for _, c := range s.halt() {
		c.close()
	}
	s.opMu.Lock()
	s.forgetHolds()
	s.opMu.Unlock()
	s.inflight.Wait()
}

// halt marks the service stopped, forgets every problem and takes every
// client out of use, returning them for the caller to close.
func (s *Service) halt() map[string]*clients {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	open := s.clients
	s.clients = map[string]*clients{}
	clear(s.problems)
	clear(s.unexplained)
	return open
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
// row is left as it is, and its next use builds a new client. The
// connection's unexplained references go with it. It reports whether a
// problem was forgotten. The caller holds opMu.
func (s *Service) stopUsing(id string) bool {
	s.mu.Lock()
	_, had := s.problems[id]
	delete(s.problems, id)
	s.forgetUnexplainedLocked(id)
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
	token, err := readToken(s.keyFile(row), row.Kind)
	token.Zero()
	return s.viewWith(row, keyfile.StatusOf(err))
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
	token, err := readToken(s.keyFile(row), row.Kind)
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
// A zero at applies only while the connection has no client. A
// settlement Stop overtook changes nothing and fails with ErrStopped. The
// caller
// holds opMu, which serializes settlements and keeps the connection in
// use during record.
func (s *Service) settle(id, kind string, at stamp, callErr error, record func() error) (bool, error) {
	s.mu.Lock()
	applies, err := s.appliesLocked(id, at)
	s.mu.Unlock()
	if !applies {
		return false, err
	}
	if callErr == nil && record != nil {
		if err := record(); err != nil {
			return false, err
		}
	}
	if s.beforePublish != nil {
		s.beforePublish()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Checked again where it publishes: Stop takes no opMu and may have
	// run since. A verification row record wrote stays; it is true.
	if applies, err := s.appliesLocked(id, at); !applies {
		return false, err
	}
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

// appliesLocked reports whether the outcome of the call begun at at on
// connection id may be published, and ErrStopped when Stop overtook it.
// The caller holds mu.
func (s *Service) appliesLocked(id string, at stamp) (bool, error) {
	switch {
	case s.stopped:
		return false, ErrStopped
	case s.clients[id] != at.c:
		return false, nil
	}
	return at.c == nil || at.gen > at.c.settled, nil
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
	p, _, err := s.providerLocked(row)
	return p, err
}

// providerLocked is the provider of connection row, with the client of
// the connection itself that its calls are stamped on (begin). The caller
// holds opMu and read row under it.
func (s *Service) providerLocked(row store.TrackerConnection) (tracker.Provider, *clients, error) {
	if row.Kind == store.TrackerKindTempo {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoProvider, row.ID)
	}
	c, err := s.use(row)
	if err != nil {
		return nil, nil, err
	}
	var timeClients *clients
	if row.Kind == store.TrackerKindJira {
		timeRow, ok, err := s.tempoOf(row.ID)
		if err != nil {
			return nil, nil, err
		}
		if ok && timeRow.Enabled {
			if timeClients, err = s.use(timeRow); err != nil {
				return nil, nil, fmt.Errorf("trackerapp: the Tempo connection of %s: %w", row.ID, err)
			}
		}
	}
	p, err := s.makeProvider(row, c, timeClients)
	if err != nil {
		return nil, nil, err
	}
	return p, c, nil
}

// buildProvider is the provider over c, the client of connection row, and
// for a Jira connection tempo, its Tempo connection's client or nil.
func buildProvider(row store.TrackerConnection, c *clients, tempo *clients) (tracker.Provider, error) {
	if row.Kind == store.TrackerKindLinear {
		p, err := linearprovider.New(c.linear)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	opts := jiraprovider.Options{Jira: c.jira, Site: row.Site}
	if tempo != nil {
		opts.Tempo = tempo.tempo
	}
	p, err := jiraprovider.New(opts)
	if err != nil {
		return nil, err
	}
	return p, nil
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

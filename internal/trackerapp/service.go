package trackerapp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerdash"
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
	ProjectTrackerBoardListed(projectID, connectionID, boardID string) (string, bool, error)
	GetProject(id string) (store.Project, error)
	ListTrackerBoardChoices(projectID string) ([]store.TrackerBoardChoice, error)
	ListConnectionProjectBoards(connectionID string) ([]store.ConnectionProjectBoard, error)
	ReplaceProjectTrackerBoards(projectID string, held []store.HeldTrackerBoard, boards []store.ProjectTrackerBoard) (bool, store.TrackerBoardListing, error)
	ReadyTrackerBoards(ctx context.Context, boards []store.TrackerBoardRef) error

	TicketConnection(ticket store.TicketRef) (string, string, error)
	GetThreadTicket(threadID string) (store.ThreadTicket, bool, error)
	ListThreadTickets(threadIDs []string) (map[string]store.ThreadTicket, error)
	SetThreadTicket(w store.ThreadTicketWrite) (store.ThreadTicketSet, error)
	ClearThreadTicket(threadID string) (store.ThreadTicketClear, error)
	ListThreadsLinkedToSite(kind, site string) ([]string, error)
	ListUnkeptLinkedIssues(tickets []store.TicketRef) ([]store.MirrorIssueRef, error)
	TicketsByKey(keys, prefer []string) (map[string][]store.KeyedTicket, error)

	ClaimAgentWrite(w store.AgentWrite, since int64) (store.AgentWrite, bool, error)
	ResendAgentWrite(id, sent int64) error
	RecordAgentWrites(sent int64, ws []store.AgentWrite) error
	// The time draft (store tracker_time_entries.go).
	ReplaceAgentTimeDays(connection, author string, entries []store.TimeEntry, now int64) (store.TimeDraftWrite, error)
	TimeEntriesByID(connection string, ids []int64) ([]store.TimeEntry, error)
	ThreadTimeEntries(thread, call string) ([]store.TimeEntry, error)
	OpenTimeEntries(connection, author string, days []string) ([]store.TimeEntry, error)
	EditTimeEntry(connection, author string, ref store.TimeEntryRef, ch store.TimeEntryChange, now int64) (store.TimeEntryEdited, bool, error)
	RemoveTimeEntries(connection, author string, refs []store.TimeEntryRef) ([]store.TimeEntry, error)
	MarkTimeEntriesSent(connection string, refs []store.TimeEntryRef, now int64) ([]store.TimeEntry, error)
	SettleTimeEntries(connection string, outcomes []store.TimeEntrySettle, now int64) ([]store.TimeEntry, error)
	RecoverEarlierTimeSends(now int64) ([]store.TimeEntry, error)
	SettleAgentWrite(id int64, issue, key, comment string) error
	AgentWritesOf(ticket store.TicketRef) ([]store.AgentWriteMark, error)
}

// Deps are the service's capabilities. Every field is required but
// Cadence.
type Deps struct {
	Store Store
	// Changed is told after every change a list of connections would show
	// differently: a row, a key file the service created or removed, a
	// verification, a problem. It must not block.
	Changed func()
	// CredentialsDir is the owner-only folder the key files live in
	// (keyfile.DirName under the app's config folder).
	CredentialsDir string
	// ConfigDir is the app's config folder, whose project directories
	// (project.ConfigDir) hold each project's tracker setup file.
	ConfigDir string
	// SetupChanged is told after every change of a project's tracker
	// setup as it is stored (setup.go). It must not block.
	SetupChanged func()
	// Origin says where a connection's client connects.
	Origin Origin
	// Version is the app version the clients send in their User-Agent.
	Version string
	// Now is the clock verification times are read from.
	Now func() time.Time
	// Life ends the calls the service makes when the app stops.
	Life context.Context

	// EmitBoard and EmitIssue deliver a subscription's events, in the order
	// of their sequence numbers. They are called with the service's
	// subscription lock held: they must not block or call the service.
	EmitBoard func(BoardEvent)
	EmitIssue func(IssueEvent)
	// TimeEntriesChanged is told each change of a time draft's entries
	// (time_draft.go). It must not block or call the service.
	TimeEntriesChanged func(TimeEntriesEvent)
	// Settled waits until the first client's reads have settled, or ctx
	// ends; the linked-issues loop waits for it before its first round.
	Settled func(ctx context.Context) error
	// ClientConnected reports whether a client is connected; the linked
	// loop skips its rounds while none is.
	ClientConnected func() bool
	// LoopFailed is told each failure of the linked loop's work (a mirror
	// cleanup, a linked-issues tick that failed outside the tracker),
	// which the loop also logs and survives. It must not block.
	LoopFailed func(error)
	// LinksChanged is told the threads whose ticket link a write of the
	// service moved, or whose link a connection's change moves (what it
	// shows, or whether it is current), so their rows are sent again. It
	// must not block.
	LinksChanged func(threadIDs []string)
	// Cadence is the sync intervals; its zero value is production's.
	Cadence Cadence
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
	// ErrIssueNotFound is an issue the tracker did not return: deleted, or
	// not visible to the token's user.
	ErrIssueNotFound = errors.New("trackerapp: the tracker did not return the issue")
	// ErrNotOnBoard is an issue a rows read of a board did not return a
	// row of: the board no longer lists it, or the tracker no longer shows
	// it to the token's user.
	ErrNotOnBoard = errors.New("trackerapp: the board no longer lists the issue")
)

// Service owns the tracker connections of this computer: their rows'
// changes, their key files, their clients and the last problem each one
// had. Make it with New; Stop releases it.
type Service struct {
	store          Store
	changed        func()
	credentialsDir string
	configDir      string
	setupChanged   func()
	origin         Origin
	version        string
	now            func() time.Time
	// steady is the clock the history wait is measured on (store
	// MirrorHistory.AnsweredAt): the wall clock when the service was made
	// plus the monotonic time since, so a step of the wall clock while it
	// runs moves no wait. steadyFrom is its reading then, in Unix ms: an
	// answer stored at or after it arrived in this process, so the time
	// since it is known to have elapsed. Tests replace both (useClock).
	steady     func() time.Time
	steadyFrom int64
	life       context.Context
	// halting ends at Stop (halt) or with life: the mirror cleanup runs
	// under it, so Stop ends a cleanup in flight before its next
	// transaction rather than waiting for all of it. A run's calls end
	// with life alone: one that Stop meets answers what it did.
	halting context.Context
	endHalt context.CancelFunc

	// setupMu guards setups, each used project's tracker setup by slug
	// (setup.go). It is held across the setup file's read and write, and
	// is taken last: a pins event reads the setup under it inside subMu
	// (tellPins), and no other of the service's locks is taken while it
	// is held.
	setupMu sync.Mutex
	setups  map[string]*heldSetup

	// propMu guards proposals, the widgets agents proposed on each
	// project's boards (dashboards.go). A Keep holds it across its write
	// of the setup file and tells the board's subscriptions after, so
	// propMu is taken before setupMu and subMu and never while holding
	// either or any other of the service's locks.
	propMu    sync.Mutex
	proposals map[proposalKey][]heldProposal

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
	problems map[string]Problem
	stopped  bool
	inflight sync.WaitGroup
	// locks are the scope and history locks of the ticks in flight
	// (tick.go).
	locks map[lockKey]*scopeLock

	// logf records what fails after the operation that caused it has
	// succeeded: a mirror's drain. Tests replace it.
	logf func(format string, args ...any)

	// beforePublish, set only by tests, runs in settle between its record
	// and the publication, where Stop may overtake a settlement.
	beforePublish func()
	// afterSetupWrite, set only by tests, runs in setupMoved before it
	// tells anyone of a setup write, where another write may commit and
	// be told first.
	afterSetupWrite func()
	// afterIdentity, set only by tests, runs in Verify between its
	// tracker call and its settlement, where other calls may change the
	// token the client holds.
	afterIdentity func()
	// makeProvider builds the provider of a connection in use from its
	// client; tests replace it with a scripted provider.
	makeProvider func(row store.TrackerConnection, c *clients, tempo *clients) (tracker.Provider, error)

	emitBoard       func(BoardEvent)
	emitIssue       func(IssueEvent)
	emitTimeEntries func(TimeEntriesEvent)
	settled         func(context.Context) error
	clientConnected func() bool
	loopFailed      func(error)
	linksChanged    func([]string)
	cadence         Cadence
	// dash evaluates the boards' dashboards (dashboards.go).
	dash *trackerdash.Evaluator
	// digestSeed keys the digests an open ticket keeps of the view it last
	// published (ticketPump.digest).
	digestSeed maphash.Seed
	// session names this service's open tickets in the mirror, each a
	// pump of it (bindTickets): the rows of another session were left by
	// a process that ended, and the first run drops them; earlier.done
	// reports that delete made, and the time sends that process left
	// recovered (recoverEarlier).
	session int64
	earlier struct {
		sync.Mutex
		done bool
	}
	// agentSends are the agents' comments and creates being sent now
	// (agent_writes.go).
	agentSends agentSends

	// owedDrops are the keeps of ended pumps whose drop failed, owed until
	// one commits (dropKeep, payOwedDrops). Guarded by opMu.
	owedDrops []owedDrop

	// subMu is the one lock of the subscriptions, their pumps and the
	// linked loop (subscriptions.go). It is held for map and flag work and
	// for the emit callbacks, never across a store or tracker call; opMu
	// is taken before it, setupMu only inside it (tellPins), and mu is
	// never held with it.
	subMu sync.Mutex
	subs  subscriptions
	// workers joins every pump goroutine and the linked loop; Stop waits
	// for it after stopping them.
	workers sync.WaitGroup
}

// New makes the service. It refuses a missing dependency, and reads,
// builds and starts nothing else: a client is built on a connection's
// first use, a pump on a subscription's first handle, and the linked loop
// after StartLinkedSync.
func New(deps Deps) (*Service, error) {
	switch {
	case deps.Store == nil, deps.Changed == nil, deps.Origin == nil, deps.Now == nil, deps.Life == nil:
		return nil, errors.New("trackerapp: Deps needs Store, Changed, Origin, Now and Life")
	case deps.EmitBoard == nil, deps.EmitIssue == nil, deps.Settled == nil, deps.ClientConnected == nil, deps.LoopFailed == nil,
		deps.LinksChanged == nil:
		return nil, errors.New("trackerapp: Deps needs EmitBoard, EmitIssue, Settled, ClientConnected, LoopFailed and LinksChanged")
	case deps.CredentialsDir == "", deps.ConfigDir == "":
		return nil, errors.New("trackerapp: Deps needs CredentialsDir and ConfigDir")
	case deps.SetupChanged == nil:
		return nil, errors.New("trackerapp: Deps.SetupChanged is required")
	case deps.Version == "":
		return nil, errors.New("trackerapp: Deps.Version is required for the clients' User-Agent")
	}
	var session [8]byte
	_, _ = rand.Read(session[:]) // crypto/rand.Read never fails; it crashes instead
	// time.Since reads the monotonic clock of mono, which deps.Now's value
	// may not carry.
	mono, wall := time.Now(), deps.Now()
	halting, endHalt := context.WithCancel(deps.Life)
	s := &Service{
		store: deps.Store, changed: deps.Changed, credentialsDir: deps.CredentialsDir, configDir: deps.ConfigDir,
		setupChanged: deps.SetupChanged, setups: map[string]*heldSetup{},
		origin: deps.Origin, version: deps.Version, now: deps.Now, life: deps.Life, halting: halting, endHalt: endHalt,
		steady: func() time.Time { return wall.Add(time.Since(mono)) }, steadyFrom: wall.UnixMilli(),
		clients: map[string]*clients{}, problems: map[string]Problem{},
		locks: map[lockKey]*scopeLock{}, holds: map[string]*connHold{},
		logf:      log.Printf,
		emitBoard: deps.EmitBoard, emitIssue: deps.EmitIssue, emitTimeEntries: deps.TimeEntriesChanged, settled: deps.Settled,
		clientConnected: deps.ClientConnected, loopFailed: deps.LoopFailed, linksChanged: deps.LinksChanged, cadence: deps.Cadence.withDefaults(),
		digestSeed: maphash.MakeSeed(), session: int64(binary.LittleEndian.Uint64(session[:]) >> 1),
		dash: trackerdash.NewEvaluator(deps.Now),
	}
	s.makeProvider = func(row store.TrackerConnection, c, tempo *clients) (tracker.Provider, error) {
		return buildProvider(row, c, tempo, s.window)
	}
	return s, nil
}

// Stop stops every pump and the linked loop, which cancels their ticks in
// flight, closes every client, which ends the tracker calls in flight, and
// waits for the service's calls in flight, its pumps and its loop to
// return. Subscribers are told nothing. Every later call fails with
// ErrStopped. Stop may be called more than once.
func (s *Service) Stop() {
	s.haltSubscriptions()
	for _, c := range s.halt() {
		c.close()
	}
	s.opMu.Lock()
	s.forgetHolds()
	s.opMu.Unlock()
	s.inflight.Wait()
	s.workers.Wait()
	s.payOwedDrops("the next process drops it")
}

// halt marks the service stopped, forgets every problem and takes every
// client out of use, returning them for the caller to close.
func (s *Service) halt() map[string]*clients {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	s.endHalt()
	open := s.clients
	s.clients = map[string]*clients{}
	clear(s.problems)
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
// key file is and the file's state, whether the last verification checked
// the token in the file now, and its last problem. Never the token.
type Connection struct {
	store.TrackerConnection
	KeyFilePath string
	KeyFile     keyfile.Status
	// CheckedTokenInFile is true while the key file holds the token the
	// row's verification checked.
	CheckedTokenInFile bool
	// Problem is the connection's last failure; nil when its last call
	// succeeded, or none failed since it was last turned on.
	Problem *Problem
}

// view is row as Connection, its key file inspected now.
func (s *Service) view(row store.TrackerConnection) Connection {
	file, found := s.readKeyFile(row)
	return s.viewWith(row, file, found)
}

// readKeyFile reads row's key file once: its state, and the digest of the
// token it holds (zero when it holds none).
func (s *Service) readKeyFile(row store.TrackerConnection) (keyfile.Status, store.TrackerTokenDigest) {
	token, err := readToken(s.keyFile(row), row.Kind)
	var found store.TrackerTokenDigest
	if err == nil {
		found = tokenDigest(row.ID, token.Reveal())
	}
	token.Zero()
	return keyfile.StatusOf(err), found
}

// viewWith is row as Connection with its key file in state file, holding
// the token found names (zero for none). Evidence counts for the token in
// the file only while the file holds the token it is about: the
// verification while found is the token it checked, the problem by its
// evidence (Problem.Current).
func (s *Service) viewWith(row store.TrackerConnection, file keyfile.Status, found store.TrackerTokenDigest) Connection {
	v := Connection{TrackerConnection: row, KeyFilePath: s.keyFilePath(row.ID), KeyFile: file, CheckedTokenInFile: sameToken(row.VerifiedToken, found)}
	s.mu.Lock()
	if p, ok := s.problems[row.ID]; ok {
		p.Current = p.about.currentFor(found)
		v.Problem = &p
	}
	s.mu.Unlock()
	return v
}

// look is row as Connection from one read of its key file, which also
// tells the connection's client, when it is in use, whether the token in
// the file is still the one it was handed (clients.syncCredential).
func (s *Service) look(row store.TrackerConnection) Connection {
	file, found := s.readKeyFile(row)
	s.mu.Lock()
	c := s.clients[row.ID]
	s.mu.Unlock()
	if c != nil {
		c.syncCredential(found)
	}
	return s.viewWith(row, file, found)
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
// call's outcome against theirs (settle), and the client's count of token
// changes then (keyFileSource.changes), against which settle tells a
// token that changed during the call. The zero stamp is a failure to
// start using the connection at all.
type stamp struct {
	c       *clients
	gen     uint64
	changes uint64
}

// begin stamps a call about to be made with c, the client of a
// connection in use.
func (s *Service) begin(c *clients) stamp {
	changes := c.source.changeCount()
	s.mu.Lock()
	defer s.mu.Unlock()
	c.gen++
	return stamp{c: c, gen: c.gen, changes: changes}
}

// evidenceOf is what the outcome of the call begun at at, with callErr,
// is about. An outcome is about token T only when every request of its
// call carried T: a key file that gave no token; no token, for a call
// with no client or one whose client was handed none; a changed token,
// when the token the client may send changed since at (either could have
// answered, even on a client that held none at the start); otherwise the
// one token the client was handed.
func evidenceOf(at stamp, callErr error) evidence {
	if _, ok := errors.AsType[*KeyFileError](callErr); ok {
		return evidence{kind: aboutKeyFile}
	}
	if at.c == nil {
		return evidence{kind: aboutNoToken}
	}
	held, kept := at.c.source.heldSince(at.changes)
	switch {
	case !kept:
		return evidence{kind: aboutChangedToken}
	case held.Known():
		return evidence{kind: aboutToken, token: held}
	}
	return evidence{kind: aboutNoToken}
}

// settle is the one place a connection's problem is set or cleared: the
// outcome of the call on connection id, of kind, that began at at.
// callErr nil is a success, which clears the problem; record, when not
// nil, runs first with the token the call used (zero when it is not
// known, evidenceOf) and its error stops the settlement (a
// verification's row write). A failure becomes the problem as
// ClassifyFailure sorts it, bound to what it is about (evidenceOf).
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
//
// changed reports that what a list of connections shows moved: the
// problem started, changed or cleared, or record wrote. A success that
// finds no problem, and the same failure about the same token, change
// nothing, so a poll that keeps succeeding tells no one.
func (s *Service) settle(id, kind string, at stamp, callErr error, record func(verified store.TrackerTokenDigest) error) (applied, changed bool, _ error) {
	about := evidenceOf(at, callErr)
	s.mu.Lock()
	applies, err := s.appliesLocked(id, at)
	s.mu.Unlock()
	if !applies {
		return false, false, err
	}
	recorded := false
	if callErr == nil && record != nil {
		var verified store.TrackerTokenDigest
		if about.kind == aboutToken {
			verified = about.token
		}
		if err := record(verified); err != nil {
			return false, false, err
		}
		recorded = true
	}
	if s.beforePublish != nil {
		s.beforePublish()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Checked again where it publishes: Stop takes no opMu and may have
	// run since. A verification row record wrote stays; it is true.
	if applies, err := s.appliesLocked(id, at); !applies {
		return false, false, err
	}
	if at.c != nil {
		at.c.settled = at.gen
	}
	held, had := s.problems[id]
	if callErr != nil {
		p := Problem{Failure: ClassifyFailure(kind, callErr), about: about}
		s.problems[id] = p
		return true, recorded || !had || held != p, nil
	}
	delete(s.problems, id)
	return true, recorded || had, nil
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

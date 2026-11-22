package deviceclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/computerroute"
)

// SessionsDirName holds one file per backend this installation is paired
// with, beside the one device key they all present.
//
// A directory rather than a single document, because the failure mode of
// one document is the one that matters here: a rotation for backend A that
// tore mid-write would take backend B's credential with it, and both would
// then have to be paired again. One atomicfile per backend means a write
// touches exactly the session it rotates.
const SessionsDirName = "sessions"

// ErrNoSession means this profile holds no session for the backend asked
// about. A state, not a fault — it is what every unpaired installation
// answers.
var ErrNoSession = errors.New("deviceclient: this profile holds no session for that backend")

// ErrInvalidBackendID is the id a session could never be filed under: not
// a UUID-shaped name, so no profile of it can exist.
var ErrInvalidBackendID = errors.New("deviceclient: not a backend id this client can file a session under")

// Session is one paired backend as this installation holds it: what to
// dial, what to pin, and the credential pair to present.
//
// Persisted as additive JSON. Unknown fields survive changes made by this
// build, so an older client cannot erase a newer build's route or profile data.
type Session struct {
	// OwnDevice records enrollment intent; the server remains authoritative.
	OwnDevice   bool `json:"ownDevice,omitempty"`
	extraFields map[string]json.RawMessage
	// PendingNextSecret is saved before a recoverable renewal reaches the wire.
	PendingNextSecret string `json:"pendingNextSecret,omitempty"`
	// RefreshRecovery is learned from this backend, never guessed from client version.
	RefreshRecovery *bool `json:"refreshRecovery,omitempty"`
	// BackendID is the key this file is named by and the identity a
	// person names on the command line.
	BackendID string `json:"backendId"`
	// BackendName is what the backend called itself when this device
	// paired. Display only, and refreshed by nothing: it is a label, not
	// a fact this client verifies.
	BackendName string `json:"backendName,omitempty"`
	// Endpoint is the address the pairing payload named, kept in the
	// spelling it arrived in. What this client actually dials is derived
	// (dialBase), so the recorded value stays comparable with the link a
	// person still has in their scrollback.
	Endpoint string `json:"endpoint"`
	// CertFingerprint is what this device pins for the endpoint. Empty
	// means ordinary WebPKI verification, never "unverified".
	CertFingerprint string `json:"certFingerprint,omitempty"`
	// Routes are alternatives learned from a trusted bootstrap. The original
	// Endpoint and its pairing trust remain stable across route selection.
	Routes       []computerroute.Route `json:"routes,omitempty"`
	LastEndpoint string                `json:"lastEndpoint,omitempty"`

	// SessionID is stable across every rotation of this session.
	SessionID string `json:"sessionId"`
	// Credential is the signed session credential presented on every
	// request.
	Credential string `json:"credential"`
	// ExpiresAtMs is when Credential stops verifying, Unix milliseconds.
	ExpiresAtMs int64 `json:"expiresAtMs"`
	// RefreshSecret rotates the pair once, and once only. Presenting a
	// spent one reads as reuse evidence and ends the session, which is
	// why every path that touches it is single-flight and stores before
	// use (see Client.renew).
	RefreshSecret      string `json:"refreshSecret,omitempty"`
	RefreshExpiresAtMs int64  `json:"refreshExpiresAtMs,omitempty"`
	// Scopes is what the backend published this session as holding. It is
	// disclosure and never authorization — every RPC is re-checked
	// against the session row — so a hand-edited copy changes only what
	// this client believes it may offer.
	Scopes []string `json:"scopes,omitempty"`
	// Label is what this device asked to be called in the owner's device
	// list.
	Label string `json:"label,omitempty"`
	// Nickname is what the person running THIS installation calls the
	// backend, as distinct from BackendName, which is what the backend
	// calls itself. Local, editable, and never sent anywhere: two
	// machines that both answer "mac-mini" are told apart here.
	//
	// Kept in the session file rather than beside it because it belongs
	// to the same thing the file names, and a rotation carries it (see
	// Client.rotate, which copies the held session before replacing the
	// credential fields). A re-pairing does not (Redemption.Save).
	Nickname string `json:"nickname,omitempty"`
}

// LoadSession reads one backend's stored session.
func LoadSession(dir, backendID string) (Session, error) {
	path, err := sessionPath(dir, backendID)
	if err != nil {
		return Session{}, err
	}
	return readSession(path)
}

// SaveSession writes one backend's session, replacing whatever was there.
//
// Atomic by construction: a rotation that crashed mid-write would leave a
// credential pair whose two halves came from different exchanges, and the
// refresh half of that pair reads as reuse the next time it is presented.
func SaveSession(dir string, session Session) error {
	return WithSession(dir, session.BackendID, func(held *HeldSession) error {
		return held.Write(session)
	})
}

func writeSession(path string, session Session) error {
	if err := atomicfile.WriteJSON(path, session); err != nil {
		return fmt.Errorf("deviceclient: persist the session for %s: %w", session.BackendID, err)
	}
	return nil
}

// readSession decodes one session file, treating an absent file and an
// unusable one as different answers: absent is ErrNoSession, which every
// unpaired installation gives, while a file that decoded into something
// that could not be presented is an error naming the file — the state
// somebody has to be told about rather than have silently ignored.
func readSession(path string) (Session, error) {
	data, err := os.ReadFile(path)
	return decodeSession(path, data, err)
}

// decodeSession is readSession given the file's bytes as os.ReadFile
// answered them.
func decodeSession(path string, data []byte, readErr error) (Session, error) {
	if errors.Is(readErr, os.ErrNotExist) {
		return Session{}, ErrNoSession
	}
	if readErr != nil {
		return Session{}, fmt.Errorf("deviceclient: read %s: %w", path, readErr)
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return Session{}, fmt.Errorf("deviceclient: read %s: decode: %w", path, err)
	}
	if session.BackendID == "" || session.SessionID == "" || session.Credential == "" {
		return Session{}, fmt.Errorf("deviceclient: %s holds no usable credential", path)
	}
	return session, nil
}

// HeldSession is one backend's session file while WithSession holds its OS
// lock. It reads, writes and restores that file and nothing else, and only
// until the function WithSession ran returns. It is a handle: every copy
// shares one state, so none can write once the lock is released.
type HeldSession struct{ s *heldSession }

type heldSession struct {
	mu        sync.Mutex
	dir       string
	backendID string
	path      string
	// initial is the file as the lock found it: its bytes, whether it
	// existed, and the error reading it if neither could be told.
	initial      []byte
	initialFound bool
	initialErr   error
	stored       Session
	err          error
	released     bool
}

// errSessionReleased answers a HeldSession used after its lock was released.
var errSessionReleased = errors.New("deviceclient: the session file is no longer held")

// WithSession runs change on one backend's session file under that file's
// OS lock. That lock is the per-computer fence across processes: every write
// of a stored session (WithSession, SaveSession, ForgetSession,
// Redemption.Save and every client's sessionTransaction) takes it, so a
// decision about the stored session and the writes it leads to cannot have
// another process's write between them.
//
// Lock order: a caller may hold its in-process lock for the computer
// (attachedbackends' profile lock) and takes this one after it, and may take
// other files' locks inside it. A client's mutex is taken before this lock
// (sessionTransaction), so change takes no client's mutex and waits on
// nothing whose holder may hold one; the one exception is the client of a
// Redemption being saved, which nobody else holds yet. The wait for the
// lock is bounded by profileWriteTimeout.
func WithSession(dir, backendID string, change func(*HeldSession) error) (err error) {
	path, err := sessionPath(dir, backendID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), profileWriteTimeout)
	defer cancel()
	release, err := lockProfile(ctx, dir, filepath.Base(path))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	s := &heldSession{dir: dir, backendID: backendID, path: path}
	data, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		s.initial, s.initialFound = data, true
	case !errors.Is(readErr, os.ErrNotExist):
		s.initialErr = readErr
	}
	s.stored, s.err = decodeSession(path, data, readErr)
	defer func() {
		s.mu.Lock()
		s.released = true
		s.mu.Unlock()
	}()
	return change(&HeldSession{s: s})
}

// hold runs use with the held state locked, or refuses once it is released.
func (h *HeldSession) hold(use func(*heldSession) error) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if h.s.released {
		return errSessionReleased
	}
	return use(h.s)
}

// Stored is the session in the file now, as LoadSession answers it: the
// session, ErrNoSession, or the error reading the file.
func (h *HeldSession) Stored() (Session, error) {
	var stored Session
	err := h.hold(func(s *heldSession) error {
		stored = s.stored
		return s.err
	})
	return stored, err
}

// Write replaces the stored session with session. A write whose rename
// took effect but whose directory sync failed answers that error: the file
// may hold session all the same (Restore).
func (h *HeldSession) Write(session Session) error {
	return h.hold(func(s *heldSession) error {
		if session.BackendID != s.backendID {
			return fmt.Errorf("deviceclient: a session for %s cannot be filed under %s", session.BackendID, s.backendID)
		}
		err := writeSession(s.path, session)
		s.stored, s.err = readSession(s.path)
		return err
	})
}

// Remove drops the stored session, whichever pairing it holds and whether
// or not it can be read. A client built from it is retired by its owner
// (Client.Retire); one that is not finds the file gone at its next write and
// retires itself.
func (h *HeldSession) Remove() error {
	return h.hold(func(s *heldSession) error {
		err := atomicfile.Remove(s.path)
		s.stored, s.err = readSession(s.path)
		if err != nil {
			return fmt.Errorf("deviceclient: forget the session for %s: %w", s.backendID, err)
		}
		return nil
	})
}

// Restore puts the file back as the lock found it: the same bytes, or no
// file. It reports whether the file, read back, now is as it was found,
// which a write can achieve and still answer an error (a directory sync
// that failed after the rename); that error is returned too. A file that
// could not be read when the lock was taken cannot be put back.
func (h *HeldSession) Restore() (restored bool, err error) {
	err = h.hold(func(s *heldSession) error {
		if s.initialErr != nil {
			return fmt.Errorf("deviceclient: the session file for %s could not be read when it was locked, so it cannot be put back: %w", s.backendID, s.initialErr)
		}
		var writeErr error
		if !s.holdsInitial() {
			if s.initialFound {
				writeErr = atomicfile.Write(s.path, s.initial)
			} else {
				writeErr = atomicfile.Remove(s.path)
			}
		}
		restored = s.holdsInitial()
		s.stored, s.err = readSession(s.path)
		if writeErr != nil {
			return fmt.Errorf("deviceclient: restore the session file for %s: %w", s.backendID, writeErr)
		}
		if !restored {
			return fmt.Errorf("deviceclient: the session file for %s still differs from what was stored", s.backendID)
		}
		return nil
	})
	return restored, err
}

// holdsInitial reads the file back and reports whether it is as the lock
// found it.
func (s *heldSession) holdsInitial() bool {
	data, err := os.ReadFile(s.path)
	if !s.initialFound {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && bytes.Equal(data, s.initial)
}

// inProfile reports whether the held file is in profile directory dir.
func (h *HeldSession) inProfile(dir string) bool {
	return filepath.Clean(h.s.dir) == filepath.Clean(dir)
}

// ForgetSession drops one backend's stored session (HeldSession.Remove).
// The device key survives: it names the DEVICE, and the backend adopts its
// row by thumbprint when this installation pairs again.
func ForgetSession(dir, backendID string) error {
	return WithSession(dir, backendID, (*HeldSession).Remove)
}

// ListSessions reads every stored session, ordered by backend id so two
// runs print the same list.
//
// A file that cannot be read is SKIPPED rather than failing the listing:
// one damaged profile must not make the other backends unreachable, and
// the damaged one surfaces as "not paired" the moment somebody names it.
func ListSessions(dir string) ([]Session, error) {
	if dir == "" {
		return nil, errors.New("deviceclient: no profile directory to read sessions from")
	}
	root := filepath.Join(dir, SessionsDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("deviceclient: read %s: %w", root, err)
	}
	sessions := make([]Session, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		session, err := readSession(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].BackendID < sessions[j].BackendID })
	return sessions, nil
}

// Resolve picks the stored session a person named on the command line.
//
// Three spellings, tried in this order, because each is what somebody has
// in front of them at a different moment:
//
//  1. the backend id, which is what `ListSessions` prints;
//  2. the endpoint exactly as stored, which is what a paste of the
//     pairing link's address gives;
//  3. the endpoint's AUTHORITY (`host:port`), which is what somebody
//     types from memory — with or without a scheme in front of it.
//
// Deliberately not a prefix or substring match. A resolution that could
// mean two backends is a resolution that attaches to the wrong machine
// silently, so an argument matching more than one profile is an error that
// names them rather than a first-hit answer.
func Resolve(dir, target string) (Session, error) {
	wanted := strings.TrimSpace(target)
	if wanted == "" {
		return Session{}, errors.New("deviceclient: name a backend to attach to")
	}
	sessions, err := ListSessions(dir)
	if err != nil {
		return Session{}, err
	}
	if len(sessions) == 0 {
		return Session{}, ErrNoSession
	}
	for _, match := range []func(Session) bool{
		func(s Session) bool { return s.BackendID == wanted },
		func(s Session) bool { return s.Endpoint == wanted },
		func(s Session) bool { return endpointAuthority(s.Endpoint) == endpointAuthority(wanted) },
	} {
		hits := make([]Session, 0, 1)
		for _, session := range sessions {
			if match(session) {
				hits = append(hits, session)
			}
		}
		switch len(hits) {
		case 0:
		case 1:
			return hits[0], nil
		default:
			names := make([]string, 0, len(hits))
			for _, hit := range hits {
				names = append(names, hit.BackendID)
			}
			return Session{}, fmt.Errorf(
				"deviceclient: %q names more than one paired backend (%s); use the backend id",
				wanted, strings.Join(names, ", "))
		}
	}
	return Session{}, fmt.Errorf("deviceclient: %q names no paired backend: %w", wanted, ErrNoSession)
}

// endpointAuthority reduces an endpoint to `host:port` so the three
// spellings a person might type all compare equal. An input with no scheme
// is taken as the authority itself, which is the case this exists for.
func endpointAuthority(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return ""
	}
	for _, scheme := range []string{"http://", "https://", "ws://", "wss://"} {
		if rest, found := strings.CutPrefix(trimmed, scheme); found {
			trimmed = rest
			break
		}
	}
	authority, _, _ := strings.Cut(trimmed, "/")
	return authority
}

// sessionPath resolves one backend's file, refusing an id that is not a
// name.
//
// The id comes off the wire, in a payload this client did not mint, and it
// is about to become a path component. The alphabet below has no separator
// and no dot-run in it, so nothing that passes can address a directory
// other than this one — which closes the class rather than sanitising one
// spelling of it. Real backend ids are UUIDs and pass unchanged; anything
// else is refused loudly, because a profile filed under a name this client
// cannot reproduce is one it could never load again.
//
// The file name is also checked with filepath.IsLocal at the join, the
// check CodeQL's path-injection query recognizes; every read, write, lock
// and delete of the file takes its path from here.
func sessionPath(dir, backendID string) (string, error) {
	if dir == "" {
		return "", errors.New("deviceclient: no profile directory to keep sessions in")
	}
	name := backendID + ".json"
	if !validBackendID(backendID) || !filepath.IsLocal(name) {
		return "", fmt.Errorf("%w: %q", ErrInvalidBackendID, backendID)
	}
	return filepath.Join(dir, SessionsDirName, name), nil
}

// maxBackendIDLen bounds the id at far more than a UUID needs and far less
// than any filesystem's name limit.
const maxBackendIDLen = 128

func validBackendID(backendID string) bool {
	if backendID == "" || len(backendID) > maxBackendIDLen {
		return false
	}
	for _, r := range backendID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

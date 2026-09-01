package deviceclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/atomicfile"
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

// Session is one paired backend as this installation holds it: what to
// dial, what to pin, and the credential pair to present.
//
// Persisted verbatim as JSON, so every field is additive-only. A field this
// build does not know survives a rotation only if it is declared here, and
// the ones that are not are the ones a rotation legitimately replaces.
type Session struct {
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
	path, err := sessionPath(dir, session.BackendID)
	if err != nil {
		return err
	}
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
	var session Session
	found, err := atomicfile.ReadJSON(path, &session)
	if err != nil {
		return Session{}, fmt.Errorf("deviceclient: read %s: %w", path, err)
	}
	if !found {
		return Session{}, ErrNoSession
	}
	if session.BackendID == "" || session.SessionID == "" || session.Credential == "" {
		return Session{}, fmt.Errorf("deviceclient: %s holds no usable credential", path)
	}
	return session, nil
}

// ForgetSession drops one backend's stored session. The device key
// survives: it names the DEVICE, and the backend adopts its row by
// thumbprint when this installation pairs again.
func ForgetSession(dir, backendID string) error {
	path, err := sessionPath(dir, backendID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deviceclient: forget the session for %s: %w", backendID, err)
	}
	return nil
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
func sessionPath(dir, backendID string) (string, error) {
	if dir == "" {
		return "", errors.New("deviceclient: no profile directory to keep sessions in")
	}
	if !validBackendID(backendID) {
		return "", fmt.Errorf(
			"deviceclient: %q is not a backend id this client can file a session under", backendID)
	}
	return filepath.Join(dir, SessionsDirName, backendID+".json"), nil
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

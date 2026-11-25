package trackerapp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
)

// clients is the one client a connection in use holds: the jira client of
// a Jira connection, the tempo client of a Tempo connection or the linear
// client of a Linear one. Close is safe while calls are in flight: each
// client's Close cancels them, and they fail with a
// *trackerapi.NotSentError or the ended context.
type clients struct {
	jira   *jira.Client
	tempo  *tempo.Client
	linear *linear.Client
	// source is the client's credential source.
	source *keyFileSource
	// parentID is the Jira connection a Tempo connection belongs to: when
	// that one stops being used, so does this one.
	parentID string
	// gen counts the calls begun on the client (Service.begin); settled is
	// the gen of the call whose outcome was applied last (Service.settle).
	// Guarded by Service.mu.
	gen, settled uint64
}

func (c *clients) forgetCredential() {
	switch {
	case c.jira != nil:
		c.jira.ForgetCredential()
	case c.tempo != nil:
		c.tempo.ForgetCredential()
	case c.linear != nil:
		c.linear.ForgetCredential()
	}
}

// syncCredential compares the digest of the token one read of the key
// file found (zero when the read failed, which matches no token) with the
// token the client was last handed, and makes the client read the file
// again only when they differ. A list, a key file open or a verification
// thereby picks up a replaced token without disturbing calls in flight
// when nothing changed.
func (c *clients) syncCredential(found store.TrackerTokenDigest) {
	c.source.mu.Lock()
	defer c.source.mu.Unlock()
	if !c.source.handed.Known() {
		return // the client holds no credential, or one already dropped
	}
	if sameToken(c.source.handed, found) {
		return
	}
	c.source.handed = store.TrackerTokenDigest{}
	c.source.changes++
	c.forgetCredential()
}

// tokenDigest is all the service keeps of a token: SHA-256 over a fixed
// domain, the connection's ID and the token, stored only for a token a
// tracker verified, whose random length leaves the digest nothing to give
// away.
func tokenDigest(id string, token []byte) store.TrackerTokenDigest {
	h := sha256.New()
	_, _ = h.Write([]byte("agent-overflow tracker token v1\x00")) // a hash.Hash never fails a write
	_, _ = h.Write([]byte(id))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(token)
	var d store.TrackerTokenDigest
	h.Sum(d[:0])
	return d
}

// sameToken reports whether two digests name one token, in constant time;
// a zero digest names none.
func sameToken(a, b store.TrackerTokenDigest) bool {
	return a.Known() && subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (c *clients) close() {
	switch {
	case c.jira != nil:
		c.jira.Close()
	case c.tempo != nil:
		c.tempo.Close()
	case c.linear != nil:
		c.linear.Close()
	}
}

// keyFilePath is where the token of connection id lives.
func (s *Service) keyFilePath(id string) string {
	return filepath.Join(s.credentialsDir, "tracker-"+id)
}

func (s *Service) keyFile(row store.TrackerConnection) keyfile.File {
	return keyfile.New(s.keyFilePath(row.ID), keyFileLabel(row.Kind))
}

// readToken reads the token in a connection of kind's key file. A token
// shorter than trackerapi.MinSecretLen, which the transport refuses, is
// refused here, worded for the person who pasted it.
func readToken(file keyfile.File, kind string) (*secret.Bytes, error) {
	token, err := file.Read()
	if err != nil {
		return nil, err
	}
	if token.Len() < trackerapi.MinSecretLen {
		token.Zero()
		l := keyFileLabel(kind)
		return nil, fmt.Errorf("the %s API %s file holds too little to be a %s; paste the whole %s into it", l.Service, l.Kind, l.Kind, l.Kind)
	}
	return token, nil
}

// keyFileSource is the trackerapi.CredentialSource of one connection: its
// key file, read each time the client asks and presented as its tracker
// takes it.
type keyFileSource struct {
	file keyfile.File
	kind string
	// id is the connection's, which every digest of its tokens carries.
	id string
	// account is the Jira account email Basic authentication carries.
	account string

	mu sync.Mutex
	// handed is the digest of the token the source last read for the
	// client, and the client holds that token or holds nothing. It is zero
	// before the first read and after the client was made to drop it. A
	// read that found no token leaves it, and the client then holds
	// nothing: the transport drops the held token when a refresh fails. A
	// token the client refuses (Linear's workspace check) or discards (a
	// read ForgetCredential overtook) also leaves it holding nothing.
	// Never the token, and never logged.
	handed store.TrackerTokenDigest
	// changes counts the times the token the client may send stopped
	// being the one it held: syncCredential dropped it, or a read handed
	// another than handed. A first token handed after none does not count,
	// so a call that saw the count unchanged sent only the token handed
	// now (heldSince).
	changes uint64
	// reads counts the file reads the client asked for, for tests.
	reads atomic.Int64
}

// changeCount is the count heldSince compares with.
func (k *keyFileSource) changeCount() uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.changes
}

// heldSince is the digest of the token the source last handed the client
// (zero for none), and whether the token the client may send stayed that
// one since changeCount returned count. Every request runs the
// transport's credential load, which waits for a read in flight, so a
// request sent after count was taken carried that token or none.
func (k *keyFileSource) heldSince(count uint64) (store.TrackerTokenDigest, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.handed, k.changes == count
}

func (k *keyFileSource) Credential(ctx context.Context) (*trackerapi.Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k.reads.Add(1)
	token, err := readToken(k.file, k.kind)
	if err != nil {
		return nil, &KeyFileError{Path: k.file.Path(), Err: err}
	}
	d := tokenDigest(k.id, token.Reveal())
	k.mu.Lock()
	if k.handed.Known() && !sameToken(k.handed, d) {
		k.changes++
	}
	k.handed = d
	k.mu.Unlock()
	switch k.kind {
	case store.TrackerKindJira:
		return trackerapi.Basic(k.account, token), nil
	case store.TrackerKindTempo:
		return trackerapi.Bearer(token), nil
	}
	// A Linear personal API key is presented bare (linear.Options).
	return trackerapi.Key(token), nil
}

// buildClients makes the client of row. It sends nothing.
func (s *Service) buildClients(row store.TrackerConnection) (*clients, error) {
	origin, err := s.origin(row.Kind, row.Site)
	if err != nil {
		return nil, &ClientError{Kind: row.Kind, Err: err}
	}
	source := &keyFileSource{file: s.keyFile(row), kind: row.Kind, id: row.ID, account: row.Account}
	c := clients{source: source, parentID: row.ParentID}
	switch row.Kind {
	case store.TrackerKindJira:
		c.jira, err = jira.New(jira.Options{BaseURL: origin, Version: s.version, Credentials: source, Proxy: http.ProxyFromEnvironment})
	case store.TrackerKindTempo:
		c.tempo, err = tempo.New(tempo.Options{
			BaseURL: origin, Version: s.version, Credentials: source, Proxy: http.ProxyFromEnvironment,
			Issues: parentIssues{s: s, parentID: row.ParentID},
		})
	case store.TrackerKindLinear:
		// Bound to the connection's workspace: no request carries a key
		// Linear places in another one.
		c.linear, err = linear.New(linear.Options{
			BaseURL: origin, Version: s.version, Credentials: source, Proxy: http.ProxyFromEnvironment,
			Workspace: row.Site,
		})
	default:
		err = fmt.Errorf("unknown tracker kind %q", row.Kind)
	}
	if err != nil {
		return nil, &ClientError{Kind: row.Kind, Err: err}
	}
	return &c, nil
}

// parentIssues is a Tempo client's tempo.IssueIDs: the issue keys and IDs
// of the Jira connection the Tempo connection belongs to, read through
// that connection's client as it is at each call, so the Tempo client
// does not hold a Jira client that its own connection may close.
type parentIssues struct {
	s        *Service
	parentID string
}

func (p parentIssues) IDs(ctx context.Context, keys []string) (map[string]int64, error) {
	c, err := p.s.jiraClient(p.parentID)
	if err != nil {
		return nil, err
	}
	return jiraprovider.TempoIssues(c).IDs(ctx, keys)
}

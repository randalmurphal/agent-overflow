package trackerapp

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"

	"agent-overflow/internal/keyfile"
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

// keyFileSource is the trackerapi.CredentialSource of one connection: its
// key file, read on each call and presented as its tracker takes it.
type keyFileSource struct {
	file keyfile.File
	kind string
	// account is the Jira account email Basic authentication carries.
	account string
}

func (k keyFileSource) Credential(ctx context.Context) (*trackerapi.Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, err := k.file.Read()
	if err != nil {
		return nil, &KeyFileError{Path: k.file.Path(), Err: err}
	}
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
	source := keyFileSource{file: s.keyFile(row), kind: row.Kind, account: row.Account}
	var c clients
	switch row.Kind {
	case store.TrackerKindJira:
		c.jira, err = jira.New(jira.Options{BaseURL: origin, Version: s.version, Credentials: source, Proxy: http.ProxyFromEnvironment})
	case store.TrackerKindTempo:
		c.tempo, err = tempo.New(tempo.Options{
			BaseURL: origin, Version: s.version, Credentials: source, Proxy: http.ProxyFromEnvironment,
			Issues: parentIssues{s: s, parentID: row.ParentID},
		})
	case store.TrackerKindLinear:
		c.linear, err = linear.New(linear.Options{BaseURL: origin, Version: s.version, Credentials: source, Proxy: http.ProxyFromEnvironment})
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

func (p parentIssues) Keys(ctx context.Context, ids []int64) (map[int64]string, error) {
	c, err := p.s.jiraClient(p.parentID)
	if err != nil {
		return nil, err
	}
	return jiraprovider.TempoIssues(c).Keys(ctx, ids)
}

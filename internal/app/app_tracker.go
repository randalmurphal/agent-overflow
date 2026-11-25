package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/trackerapp"
)

// The issue tracker connections of this computer. trackerapp owns them;
// these are their bound methods and wire shapes. A token never crosses
// the wire: the person pastes it into the connection's key file on this
// computer (EditTrackerKeyFile), and every answer carries the file's
// state only. A connection ID is minted on one computer, so a method that
// names one is routed by its client to the computer whose answer named
// it (methodFamilies.ts, family "trackerConnection"); its generated route
// is home. TrackerConnections and AddTrackerConnection name none and go
// to the selected computer.

// TrackerConnection is one tracker connection as a client sees it.
type TrackerConnection struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Site is the Jira site's https origin, the Tempo API base URL, or the
	// Linear workspace's URL key.
	Site string `json:"site"`
	// Account is a Jira connection's email; empty for the others.
	Account string `json:"account"`
	// ParentID names the Jira connection a Tempo connection belongs to.
	ParentID string `json:"parentId,omitempty"`
	Label    string `json:"label"`
	Enabled  bool   `json:"enabled"`
	// VerifiedAccountID and VerifiedName are who the token belonged to at
	// VerifiedAt (unix milliseconds, 0 until verified).
	VerifiedAccountID string `json:"verifiedAccountId"`
	VerifiedName      string `json:"verifiedName"`
	VerifiedAt        int64  `json:"verifiedAt"`
	// TokenExpiresAt is when the person said the token expires (unix
	// milliseconds), 0 when they have not.
	TokenExpiresAt int64 `json:"tokenExpiresAt"`
	CreatedAt      int64 `json:"createdAt"`
	// KeyFilePath is where this computer keeps the connection's token.
	KeyFilePath  string        `json:"keyFilePath"`
	KeyFileState keyfile.State `json:"keyFileState"`
	// KeyFileProblem says why a refused key file is refused.
	KeyFileProblem string `json:"keyFileProblem,omitempty"`
	// Problem is the connection's last failure, held in memory until a
	// call succeeds or the connection is turned off.
	Problem *TrackerProblem `json:"problem,omitempty"`
}

// TrackerProblem is a failed tracker call: its kind (transient,
// rate_limited, setup, forbidden, tracker) and a sentence written by the
// app, never the tracker's text.
type TrackerProblem struct {
	Kind    trackerapp.FailureKind `json:"kind"`
	Message string                 `json:"message"`
}

// NewTrackerConnection is what AddTrackerConnection takes. Site is the
// Jira site (with or without https://), a Tempo API base URL, or the
// Linear workspace's URL key or address; Account is the Jira account's
// email; ParentID is the Jira connection a Tempo connection belongs to.
type NewTrackerConnection struct {
	Kind     string `json:"kind"`
	Site     string `json:"site"`
	Account  string `json:"account,omitempty"`
	ParentID string `json:"parentId,omitempty"`
	Label    string `json:"label,omitempty"`
}

func trackerConnectionDTO(c trackerapp.Connection) TrackerConnection {
	out := TrackerConnection{
		ID: c.ID, Kind: c.Kind, Site: c.Site, Account: c.Account, ParentID: c.ParentID,
		Label: c.Label, Enabled: c.Enabled,
		VerifiedAccountID: c.VerifiedAccountID, VerifiedName: c.VerifiedName, VerifiedAt: c.VerifiedAt,
		TokenExpiresAt: c.TokenExpiresAt, CreatedAt: c.CreatedAt,
		KeyFilePath: c.KeyFilePath, KeyFileState: c.KeyFile.State, KeyFileProblem: c.KeyFile.Problem,
	}
	if c.Problem != nil {
		out.Problem = &TrackerProblem{Kind: c.Problem.Kind, Message: c.Problem.Message}
	}
	return out
}

// errTrackersIsolated refuses a tracker connection in an isolated run or
// a test binary that named no fake tracker.
var errTrackersIsolated = errors.New("tracker requests are off in an isolated run with no fake tracker")

func refuseTrackerOrigin(string, string) (string, error) { return "", errTrackersIsolated }

// trackerOrigin is where this App's tracker clients connect: the
// isolated boot's choice, a refusal inside a test binary that made none,
// and the trackers themselves otherwise.
func (a *App) trackerOrigin() trackerapp.Origin {
	switch {
	case a.trackerOriginOverride != nil:
		return a.trackerOriginOverride
	case testing.Testing():
		return refuseTrackerOrigin
	}
	return trackerapp.APIOrigin
}

// initTrackers builds the tracker service once the store, the config
// folder and the app's lifetime exist.
func (a *App) initTrackers() error {
	svc, err := trackerapp.New(trackerapp.Deps{
		Store:          a.store,
		Changed:        func() { a.emit(eventchan.TrackerConnectionsUpdated, nil) },
		CredentialsDir: filepath.Join(a.configDir, keyfile.DirName),
		Origin:         a.trackerOrigin(),
		Version:        a.version,
		Now:            time.Now,
		Life:           a.appCtx,
		EmitBoard:      func(e trackerapp.BoardEvent) { a.emit(eventchan.TrackerBoardUpdated, e) },
		EmitIssue:      func(e trackerapp.IssueEvent) { a.emit(eventchan.TrackerIssueUpdated, e) },
		Settled:        a.awaitFirstReadsSettled,
		Cadence:        a.trackerCadence,
		ClientConnected: func() bool {
			bus := a.eventBus.Load()
			return bus != nil && bus.SubscriberCount() > 0
		},
		// The service logs each failure of its linked-issues loop itself.
		LoopFailed: func(error) {},
	})
	if err != nil {
		return fmt.Errorf("failed to initialise tracker connections: %w", err)
	}
	a.trackers = svc
	return nil
}

// startTrackers is the trackers' boot step: it arms the linked-issues
// loop, which waits for the first client's reads before its first round,
// and finishes, once they settle, the mirror cleanup an earlier run left
// unfinished. That cleanup is this step's alone: the loop runs one only
// after a round, while a connection is on and a client connected. Before the
// first reads settle it reads the connections once and nothing that grows
// with the mirror. A failure of either is a boot failure.
func (a *App) startTrackers() error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	ctx := a.lifeCtx()
	a.unattendedLoops.Go(func() {
		if err := a.awaitFirstReadsSettled(ctx); err != nil {
			return
		}
		if err := svc.FinishMirrorCleanup(); err != nil && !errors.Is(err, trackerapp.ErrStopped) {
			a.bootPhaseFailed(fmt.Errorf("finish the issue tracker cleanup the last run left: %w", err))
		}
	})
	if err := svc.StartLinkedSync(); err != nil {
		return fmt.Errorf("start the issue trackers' linked-issues sync: %w", err)
	}
	return nil
}

func (a *App) trackerService() (*trackerapp.Service, error) {
	if a.trackers == nil {
		return nil, errors.New("tracker connections are not available before the app has started")
	}
	return a.trackers, nil
}

// trackerCall runs one service call that answers a connection.
func (a *App) trackerCall(call func(*trackerapp.Service) (trackerapp.Connection, error)) (TrackerConnection, error) {
	svc, err := a.trackerService()
	if err != nil {
		return TrackerConnection{}, err
	}
	c, err := call(svc)
	if err != nil {
		return TrackerConnection{}, err
	}
	return trackerConnectionDTO(c), nil
}

// TrackerConnections lists this computer's tracker connections in the
// order they were added, with each key file's state and each
// connection's last problem.
//
//ao:scope tracker:read
//ao:route selected
func (a *App) TrackerConnections(ctx context.Context) ([]TrackerConnection, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	list, err := svc.Connections()
	if err != nil {
		return nil, err
	}
	out := make([]TrackerConnection, len(list))
	for i, c := range list {
		out[i] = trackerConnectionDTO(c)
	}
	return out, nil
}

// AddTrackerConnection adds a connection with an empty key file.
//
//ao:scope tracker:operate
//ao:route selected
func (a *App) AddTrackerConnection(ctx context.Context, connection NewTrackerConnection) (TrackerConnection, error) {
	return a.trackerCall(func(svc *trackerapp.Service) (trackerapp.Connection, error) {
		return svc.Add(trackerapp.NewConnection{
			Kind: connection.Kind, Site: connection.Site, Account: connection.Account,
			ParentID: connection.ParentID, Label: connection.Label,
		})
	})
}

// RenameTrackerConnection sets a connection's label.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) RenameTrackerConnection(ctx context.Context, connectionID, label string) (TrackerConnection, error) {
	return a.trackerCall(func(svc *trackerapp.Service) (trackerapp.Connection, error) {
		return svc.Rename(connectionID, label)
	})
}

// SetTrackerConnectionEnabled turns a connection on or off. Off keeps its
// row and key file and closes its client.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) SetTrackerConnectionEnabled(ctx context.Context, connectionID string, enabled bool) (TrackerConnection, error) {
	return a.trackerCall(func(svc *trackerapp.Service) (trackerapp.Connection, error) {
		return svc.SetEnabled(connectionID, enabled)
	})
}

// SetTrackerConnectionTokenExpiry records when a connection's token
// expires, in unix milliseconds, as the person read it from the tracker;
// 0 clears it.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) SetTrackerConnectionTokenExpiry(ctx context.Context, connectionID string, expiresAt int64) (TrackerConnection, error) {
	return a.trackerCall(func(svc *trackerapp.Service) (trackerapp.Connection, error) {
		return svc.SetTokenExpiry(connectionID, expiresAt)
	})
}

// RemoveTrackerConnection removes a connection, the Tempo connection that
// belongs to it, their key files and their rows.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) RemoveTrackerConnection(ctx context.Context, connectionID string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	return svc.Remove(connectionID)
}

// VerifyTrackerConnection checks a connection's token with one identity
// call to its tracker. A refused or failed call is the answer's problem,
// not an error.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) VerifyTrackerConnection(ctx context.Context, connectionID string) (TrackerConnection, error) {
	return a.trackerCall(func(svc *trackerapp.Service) (trackerapp.Connection, error) {
		return svc.Verify(ctx, connectionID)
	})
}

// EditTrackerKeyFile creates a connection's key file, empty and
// owner-only, when it is missing, and opens it in the user's editor on
// this computer.
//
//ao:scope host
//ao:route home
func (a *App) EditTrackerKeyFile(ctx context.Context, connectionID string) (TrackerConnection, error) {
	svc, err := a.trackerService()
	if err != nil {
		return TrackerConnection{}, err
	}
	c, err := svc.PrepareKeyFile(connectionID)
	if err != nil {
		return TrackerConnection{}, err
	}
	if err := a.OpenInEditor(c.KeyFilePath, 0, 0, "", ""); err != nil {
		return TrackerConnection{}, fmt.Errorf("open the key file of tracker connection %s: %w", connectionID, err)
	}
	return trackerConnectionDTO(c), nil
}

// OpenTrackerTokenPage opens, on this computer, the tracker page where
// the person makes the connection's token.
//
//ao:scope host
//ao:route home
func (a *App) OpenTrackerTokenPage(ctx context.Context, connectionID string) error {
	svc, err := a.trackerService()
	if err != nil {
		return err
	}
	page, err := svc.TokenPage(connectionID)
	if err != nil {
		return err
	}
	return a.openExternalURL(ctx, page)
}

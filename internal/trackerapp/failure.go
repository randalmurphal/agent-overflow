package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/trackerapi"
)

// FailureKind sorts a failed tracker call by what the person can do about
// it, so a surface chooses its presentation without reading the text.
type FailureKind string

const (
	// FailureTransient is a tracker the next attempt may reach: a dial, a
	// timeout, a reset, an answer cut short.
	FailureTransient FailureKind = "transient"
	// FailureRateLimited is a tracker refusing requests until a time the
	// message names.
	FailureRateLimited FailureKind = "rate_limited"
	// FailureSetup is something the person fixes on this computer: the key
	// file is missing, empty or refused, the tracker refused the token,
	// the token belongs to another workspace, or this computer cannot
	// connect to the tracker at all.
	FailureSetup FailureKind = "setup"
	// FailureForbidden is a token the tracker accepts that may not do what
	// was asked (HTTP 403).
	FailureForbidden FailureKind = "forbidden"
	// FailureTracker is every other answer: a status, an error the
	// tracker reported, a body that could not be used.
	FailureTracker FailureKind = "tracker"
	// FailureNotFound is an open ticket the tracker did not return: deleted,
	// moved out of reach, or not visible to the token's user.
	FailureNotFound FailureKind = "not_found"
	// FailureUpload is a file that stopped arriving from the client
	// uploading it before its end, so the tracker got nothing complete:
	// nothing the tracker or the connection did.
	FailureUpload FailureKind = "upload"
	// FailureInternal is a read or write of this computer's copy of the
	// tracker that failed: nothing the tracker or the person did.
	FailureInternal FailureKind = "internal"
)

// internalFailure is a subscription's failure that was not the tracker's
// (FailureInternal), at, as its subscribers read it: a sentence of this
// package's, never the error's text.
func internalFailure(at int64) *FailureView {
	return &FailureView{Kind: string(FailureInternal), Message: "Agent Overflow could not read or store its copy of the tracker on this computer; it tries again", At: at}
}

// notFoundFailure is an open ticket's failure when the tracker did not
// return it, at.
func notFoundFailure(kind string, at int64) *FailureView {
	return &FailureView{Kind: string(FailureNotFound), Message: trackerName(kind) + " did not return this issue: it was deleted, or the token's user may not see it", At: at}
}

// Failure is a failed tracker call as a person reads it. Message is
// written by this package: it never holds the token, an answer body or
// text the tracker wrote.
type Failure struct {
	Kind    FailureKind
	Message string
}

// Problem is a connection's last failure and what it is about (settle).
// Current says whether that is the token in the key file now, or no token
// at all; the view that reads the file sets it (viewWith).
type Problem struct {
	Failure
	Current bool
	about   evidence
}

// Format prints the failure and whether it is current, never what it is
// about: fmt reads an unexported field without its Format method, so the
// token digest evidence holds would print as bytes.
func (p Problem) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "{Kind:%s Message:%q Current:%t}", p.Kind, p.Message, p.Current)
}

// evidence is what an outcome is about: the token its call used, a key
// file that could not be read, no token, or a token that changed while
// the call ran, which could be either.
type evidence struct {
	kind  evidenceKind
	token store.TrackerTokenDigest
}

type evidenceKind uint8

const (
	// aboutNoToken is a call that used no token: no client could be
	// built, or it failed before reading one. It is always current.
	aboutNoToken evidenceKind = iota
	// aboutToken is a call made with the token evidence.token names.
	aboutToken
	// aboutKeyFile is a key file that gave no token (KeyFileError). It is
	// current while the file still holds none.
	aboutKeyFile
	// aboutChangedToken is a call during which the client's token
	// changed. It is never current: a check settles it.
	aboutChangedToken
)

// currentFor reports whether evidence is about the token in the key file
// now, whose digest is found (zero when the file holds none).
func (e evidence) currentFor(found store.TrackerTokenDigest) bool {
	switch e.kind {
	case aboutNoToken:
		return true
	case aboutToken:
		return sameToken(e.token, found)
	case aboutKeyFile:
		return !found.Known()
	case aboutChangedToken:
	}
	return false
}

// CallFailure is a tracker call of a form read, a download or a write's
// start that failed, with the Failure a person reads. It unwraps to the
// call's error.
type CallFailure struct {
	Failure Failure
	Err     error
}

func (e *CallFailure) Error() string { return e.Err.Error() }
func (e *CallFailure) Unwrap() error { return e.Err }

// callFailure is err, a failed call to a connection of kind, as a
// CallFailure.
func callFailure(kind string, err error) error {
	return &CallFailure{Failure: ClassifyFailure(kind, err), Err: err}
}

// KeyFileError is a key file that could not give a credential: missing,
// empty, refused or unreadable. Its message is keyfile's, which names the
// file by its label and never quotes its contents.
type KeyFileError struct {
	// Path is the key file.
	Path string
	Err  error
}

func (e *KeyFileError) Error() string { return e.Err.Error() }
func (e *KeyFileError) Unwrap() error { return e.Err }

// wrongWorkspace is the message of a Linear key that belongs to the
// workspace got instead of want. got is named only when it reads as a URL
// key, so no other text Linear sent reaches the message.
func wrongWorkspace(want, got string) string {
	got = strings.ToLower(got)
	if !isLinearURLKey(got) {
		return fmt.Sprintf("this Linear API key belongs to another workspace than %s; make a key in %s", want, want)
	}
	return fmt.Sprintf("this Linear API key belongs to the workspace %s, not %s; make a key in %s", got, want, want)
}

// ClientError is a tracker client this computer could not build: the
// origin function refused the connection (an isolated run with no fake
// tracker), or the client refused its configuration. Nothing was sent.
type ClientError struct {
	Kind string
	Err  error
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("Agent Overflow cannot connect to %s from this computer: %v", trackerName(e.Kind), e.Err)
}

func (e *ClientError) Unwrap() error { return e.Err }

// ClassifyFailure turns the error of a call to a connection of kind (a
// store.TrackerKind*) into a Failure. The key file errors, a refused
// token and a key for another workspace are setup; the transport's typed
// errors choose the other kinds. The message is built from the error's
// type and status alone, so neither the token nor tracker text reaches
// it.
func ClassifyFailure(kind string, err error) Failure {
	name := trackerName(kind)
	var keyErr *KeyFileError
	var wrong *linear.WrongWorkspaceError
	var client *ClientError
	var auth *trackerapi.AuthError
	var limited *trackerapi.RateLimitedError
	var status *trackerapi.StatusError
	var tooLarge *trackerapi.BodyTooLargeError
	var transient *trackerapi.TransientError
	var notSent *trackerapi.NotSentError
	var source *trackerapi.SourceError
	var cut *trackerapi.CutError
	switch {
	case pokerOwn(err):
		return pokerFailure(err)
	case errors.As(err, &source):
		return Failure{Kind: FailureUpload, Message: "the file stopped arriving before its end, so " + name + " stored none of it; attach it again"}
	case errors.As(err, &cut):
		return Failure{Kind: FailureTransient, Message: "sending the file to " + name + " stopped before it finished, so nothing was attached; attach it again"}
	case errors.As(err, &keyErr):
		return Failure{Kind: FailureSetup, Message: keyErr.Error()}
	case errors.As(err, &wrong):
		return Failure{Kind: FailureSetup, Message: wrongWorkspace(wrong.Want, wrong.Got)}
	case errors.As(err, &client):
		return Failure{Kind: FailureSetup, Message: client.Error()}
	case errors.As(err, &limited):
		return Failure{Kind: FailureRateLimited, Message: fmt.Sprintf("%s is limiting requests until %s", name, limited.Until.UTC().Format(time.RFC3339))}
	case errors.As(err, &auth):
		return Failure{Kind: FailureSetup, Message: refusedToken(kind, auth.Tracker)}
	case errors.As(err, &status) && status.Status == http.StatusForbidden:
		return Failure{Kind: FailureForbidden, Message: name + " refused access (HTTP 403): the token may not make this request"}
	case errors.As(err, &status) && errors.Is(err, tracker.ErrRuleRefused):
		// The reconcile waits a reconcile interval for this refusal, from
		// when it was stored, and longer while a delta walk defers it, so
		// the sentence names no time.
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf("%s refused the query for this board's issues (HTTP %d). "+
			"Refresh to try again now. Agent Overflow tries again by itself later while the board is open", name, status.Status)}
	case errors.As(err, &status):
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf("%s answered HTTP %d", name, status.Status)}
	case errors.As(err, &tooLarge):
		return Failure{Kind: FailureTracker, Message: name + " sent an answer larger than Agent Overflow reads"}
	case errors.Is(err, trackerapi.ErrCredentialChanged):
		// The token in the key file was replaced while the answer was
		// read; asking again uses the new one.
		return Failure{Kind: FailureTransient, Message: name + " answered as the token in the key file was being replaced; try again"}
	case errors.As(err, &transient), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &notSent) && !errors.Is(err, trackerapi.ErrInvalidInput):
		return Failure{Kind: FailureTransient, Message: name + " could not be reached; try again"}
	}
	return Failure{Kind: FailureTracker, Message: name + " answered in a way Agent Overflow could not use"}
}

// refusedToken is the sentence of a token refused by refuser, the name of
// the client that sent it (a store.TrackerKind*: each client is named by
// its kind), on a connection of kind. A Jira connection's time goes to
// its linked Tempo connection, whose own key file holds that token.
func refusedToken(kind, refuser string) string {
	if refuser == "" {
		refuser = kind
	}
	file := "the key file"
	if refuser != kind {
		file = "the key file of the linked " + trackerName(refuser) + " connection"
	}
	msg := trackerName(refuser) + " refused the token in " + file + "; put a valid token in it"
	if refuser == store.TrackerKindJira {
		// A scoped token answers only through Atlassian's gateway, and the
		// client calls the site (tracker-api-measurements.md).
		msg += ". A token made with Create API token with scopes cannot reach the Jira site: make one with Create API token"
	}
	if refuser == store.TrackerKindTempo {
		msg += ", and check that the region is the one the site's Tempo data is in"
	}
	return msg
}

// trackerName is how messages name a tracker kind.
func trackerName(kind string) string {
	switch kind {
	case store.TrackerKindJira:
		return "Jira"
	case store.TrackerKindTempo:
		return "Tempo"
	case store.TrackerKindLinear:
		return "Linear"
	}
	return "the tracker"
}

// keyFileLabel names a connection's key file in every message.
func keyFileLabel(kind string) keyfile.Label {
	if kind == store.TrackerKindLinear {
		return keyfile.Label{Service: "Linear", Kind: keyfile.APIKey}
	}
	return keyfile.Label{Service: trackerName(kind), Kind: keyfile.APIToken}
}

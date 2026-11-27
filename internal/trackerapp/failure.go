package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
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
// it. An answer names the tracker that gave it (a Jira connection's
// linked Tempo connection answers for its time); a call the caller marked
// with the tracker it went to (answeredBy) names that tracker when it
// could not be read, reached or answered as the token changed; any other
// failure names the connection's kind.
func ClassifyFailure(kind string, err error) Failure {
	name := trackerName(kind)
	answered := func(responder string) string {
		if responder == "" {
			return name
		}
		return trackerName(responder)
	}
	// The tracker a call that carries none of its own was made to.
	responder := ""
	if by, ok := errors.AsType[*answeredBy](err); ok {
		responder = by.tracker
	}
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
	var devShape *jira.DevShapeError
	var devErrors *jira.DevStatusError
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
		return Failure{Kind: FailureRateLimited, Message: fmt.Sprintf("%s is limiting requests until %s", answered(limited.Tracker), limited.Until.UTC().Format(time.RFC3339))}
	case errors.As(err, &auth):
		return Failure{Kind: FailureSetup, Message: refusedToken(kind, auth.Tracker)}
	case errors.As(err, &status) && status.Status == http.StatusForbidden:
		return Failure{Kind: FailureForbidden, Message: answered(status.Tracker) + " refused access (HTTP 403): the token may not make this request"}
	case errors.As(err, &status) && errors.Is(err, tracker.ErrRuleRefused):
		// The reconcile waits a reconcile interval for this refusal, from
		// when it was stored, and longer while a delta walk defers it, so
		// the sentence names no time.
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf("%s refused the query for this board's issues (HTTP %d). "+
			"Refresh to try again now. Agent Overflow tries again by itself later while the board is open", answered(status.Tracker), status.Status)}
	case errors.As(err, &status):
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf("%s answered HTTP %d", answered(status.Tracker), status.Status)}
	case errors.As(err, &devShape):
		return Failure{Kind: FailureTracker, Message: "Jira answered in a form Agent Overflow does not know (" + devShape.Why +
			"); Jira does not document these endpoints, and they may have changed"}
	case errors.As(err, &devErrors):
		return Failure{Kind: FailureTracker, Message: devStatusMessage(devErrors.Errors, devErrors.ConfigErrors)}
	case errors.As(err, &tooLarge):
		return Failure{Kind: FailureTracker, Message: answered(tooLarge.Tracker) + " sent an answer larger than Agent Overflow reads"}
	case errors.Is(err, trackerapi.ErrCredentialChanged):
		// The token in the key file was replaced while the answer was
		// read; asking again uses the new one.
		return Failure{Kind: FailureTransient, Message: answered(responder) + " answered as the token in the key file was being replaced; try again"}
	case errors.As(err, &transient), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &notSent) && !errors.Is(err, trackerapi.ErrInvalidInput):
		return Failure{Kind: FailureTransient, Message: answered(responder) + " could not be reached; try again"}
	}
	return Failure{Kind: FailureTracker, Message: answered(responder) + " answered in a way Agent Overflow could not use"}
}

// answeredBy is err, a call's failure that carries no tracker of its own
// (no status, an answer that could not be read), with the tracker that
// answered it, a store.TrackerKind*: a Jira connection's worklog read
// answered by its Tempo connection.
type answeredBy struct {
	tracker string
	err     error
}

func (e *answeredBy) Error() string { return e.err.Error() }
func (e *answeredBy) Unwrap() error { return e.err }

// timeAnswered is err, a failure of a connection's time whose tracker is
// timeBy (a store.TrackerKind*; empty for the connection's own), marked
// with that tracker, except a request the time made to Jira
// (tracker.ErrWorklogJira). Nil stays nil.
func timeAnswered(timeBy string, err error) error {
	if err == nil || timeBy == "" || errors.Is(err, tracker.ErrWorklogJira) {
		return err
	}
	return &answeredBy{tracker: timeBy, err: err}
}

// timeAnsweredParts is res with each worklog part's failure marked as
// timeAnswered marks it, so its sentence and the connection's problem
// name the tracker that failed.
func timeAnsweredParts(timeBy string, res tracker.Result) tracker.Result {
	if timeBy == "" {
		return res
	}
	parts := slices.Clone(res.Parts)
	for i, p := range parts {
		if p.Step != tracker.StepWorklog || p.Outcome.Err() == nil {
			continue
		}
		err := timeAnswered(timeBy, p.Outcome.Err())
		switch p.Outcome.State() {
		case tracker.StateRefused:
			r, _ := p.Outcome.Refusal()
			r.Err = err
			parts[i].Outcome = tracker.Refused(r)
		case tracker.StateUnknown:
			parts[i].Outcome = tracker.Unknown(err)
		case tracker.StateNotSent:
			parts[i].Outcome = tracker.NotSent(err)
		case tracker.StateApplied:
			// An applied part has no error, so the loop skipped it.
		}
	}
	res.Parts = parts
	return res
}

// responderOf is the tracker kind err names: the one it was marked as
// answered by, else kind.
func responderOf(kind string, err error) string {
	if by, ok := errors.AsType[*answeredBy](err); ok {
		return by.tracker
	}
	return kind
}

// worklogRefusal is the failure of a worklog read of author's days from
// to to that the tracker refused for what it asked, and true; false for
// any other outcome. Its sentence names the tracker that answered, what
// it refused (the person, the days) and what to do, from the request and
// the status alone. A Tempo that answers another person's worklogs with
// none, rather than 403, is not told apart from no time logged.
func worklogRefusal(author tracker.UserID, from, to tracker.Date, err error) (Failure, bool) {
	if errors.Is(err, tracker.ErrNoTimeZone) {
		// Jira answered the lookup; the person's profile hides the zone
		// that places their time on days. Only a Jira profile has one.
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf(
			"Jira does not show which time zone %s is in, so Agent Overflow cannot tell which day their time falls on; "+
				"choose another person, or ask them to show their time zone in their Atlassian profile", author)}, true
	}
	status, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok {
		return Failure{}, false
	}
	name := trackerName(status.Tracker)
	switch {
	case errors.Is(err, tracker.ErrWorklogAuthor) && status.Status == http.StatusNotFound:
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf(
			"%s has no person with account ID %s, or the token's user may not see them; choose another person", name, author)}, true
	case errors.Is(err, tracker.ErrWorklogAuthor) && status.Status == http.StatusForbidden:
		return Failure{Kind: FailureForbidden, Message: fmt.Sprintf(
			"%s refused to look up the person %s (HTTP 403): the token's user needs the Browse users and groups permission to read another person's time",
			name, author)}, true
	case status.Tracker == store.TrackerKindTempo && status.Status == http.StatusForbidden:
		return Failure{Kind: FailureForbidden, Message: fmt.Sprintf(
			"Tempo refused the worklogs of %s (HTTP 403): the linked Tempo connection's token may not read that person's time", author)}, true
	case status.Tracker == store.TrackerKindTempo && status.Status == http.StatusBadRequest:
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf(
			"Tempo refused the worklogs of %s from %s to %s (HTTP 400); check the person and the days", author, from, to)}, true
	case errors.Is(err, tracker.ErrWorklogAuthor) && status.Status == http.StatusBadRequest:
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf(
			"%s refused the account ID %s (HTTP 400); choose another person", name, author)}, true
	case errors.Is(err, tracker.ErrWorklogSearch) && status.Status == http.StatusBadRequest:
		return Failure{Kind: FailureTracker, Message: fmt.Sprintf(
			"Jira refused the search for %s's worklogs from %s to %s (HTTP 400); check the person and the days", author, from, to)}, true
	}
	return Failure{}, false
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

// devStatusMessage says a development panel answer that named errors, by
// their count alone: the classifier's sentences hold no tracker text.
func devStatusMessage(errs, configErrs int) string {
	count := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	var parts []string
	if errs > 0 {
		parts = append(parts, count(errs, "error reading the site's forge integration", "errors reading the site's forge integration"))
	}
	if configErrs > 0 {
		parts = append(parts, count(configErrs, "error in its configuration", "errors in its configuration"))
	}
	return "Jira reported " + strings.Join(parts, " and ") + "; the ticket's development panel in Jira may say more"
}

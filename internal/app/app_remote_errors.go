package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

// remoteContextError marks an error that already carries its operation
// context, so a caller wrapping an inner call's result never repeats it.
type remoteContextError struct{ error }

func (e remoteContextError) Unwrap() error { return e.error }

// remoteRef is the computer and job an error is about. The model retries by
// id; a person knows the computer and the job by name, so a message names
// both when the names are known here.
type remoteRef struct {
	ComputerID   string
	ComputerName string
	RequestID    string
	Label        string
}

// remoteRef resolves the names this computer holds for a computer and one of
// its jobs. Saved profiles and the watch table only; it never probes a peer.
func (a *App) remoteRef(computerID, requestID string) remoteRef {
	ref := remoteRef{ComputerID: computerID, RequestID: requestID}
	if !entityid.Valid(computerID) {
		return ref
	}
	ref.ComputerName = a.remoteComputerNames()[computerID]
	if entityid.Valid(requestID) && a.store != nil {
		if w, err := a.store.GetRemoteWatch(computerID, requestID); err == nil {
			ref.Label = w.Label
		}
	}
	return ref
}

// remoteOperationError names the operation, computer and job a tool refusal
// addressed and wraps err for the model. Names are resolved only for an
// actual error; wrapping is idempotent.
func (a *App) remoteOperationError(action, computerID, requestID string, err error) error {
	var done remoteContextError
	if err == nil || errors.As(err, &done) {
		return err
	}
	return remoteOperationErrorFor(action, a.remoteRef(computerID, requestID), err)
}

// Never infer acceptance from an RPC failure. The chosen request ID is the
// recovery handle even when the destination ran the command but lost its reply.
// Wrapping is idempotent: the innermost call names the operation.
func remoteOperationErrorFor(action string, ref remoteRef, err error) error {
	var done remoteContextError
	if err == nil || errors.As(err, &done) {
		return err
	}
	code, message, uncertain := remoteErrorDetails(action, err)
	prefix := "Remote " + action
	if entityid.Valid(ref.ComputerID) {
		if ref.ComputerName != "" {
			prefix += " on " + ref.ComputerName + " (computer " + ref.ComputerID + ")"
		} else {
			prefix += " on computer " + ref.ComputerID
		}
	}
	if entityid.Valid(ref.RequestID) {
		if ref.Label != "" {
			prefix += " for " + strconv.Quote(ref.Label) + " (request " + ref.RequestID + ")"
		} else {
			prefix += " for request " + ref.RequestID
		}
	}
	if entityid.Valid(ref.RequestID) {
		switch {
		case uncertain:
			message += " Check remote_status with these same IDs."
			if action == "run" {
				message += " If necessary, retry identical arguments with the SAME request_id; a new ID could run the command twice."
			}
			if action == "cancel" {
				message += " Cancellation is not confirmed; if the job is still running, retry remote_cancel with these IDs."
			}
		case remotePairingCode(code):
			message += " Keep these IDs and retry with them once the computer is reconnected."
		}
	}
	return remoteContextError{errorsx.Public(code, fmt.Sprintf("%s: %s", prefix, message), err)}
}

// remotePairingCode reports the refusals a reconnect resolves; the job, if
// accepted, is unaffected and keeps its ids.
func remotePairingCode(code string) bool {
	switch code {
	case "remote_not_paired", "remote_pairing_expired", "remote_pairing_pending", "remote_pairing_unavailable", "remote_connection_failed", "remote_request_timeout":
		return true
	}
	return false
}

// remoteErrorDetails is the public code and message for err, with no
// operation or id prefix: the text a person reads beside a row that already
// names the computer and the job. A cause with no reviewed public text stays
// in the host log behind a reference. uncertain means the destination may
// have acted on the request, which a call the carrier never sent
// (attachedbackends.NotSent) rules out.
func remoteErrorDetails(action string, err error) (code, message string, uncertain bool) {
	var done remoteContextError
	if errors.As(err, &done) {
		code, message, _ = errorsx.PublicDetails(err)
		return code, message, false
	}
	var known bool
	code, message, known = errorsx.PublicDetails(err)
	var remote *rpcclient.Error
	if !known && errors.As(err, &remote) {
		code = remote.Code
		switch {
		case strings.HasPrefix(code, "remote_"), code == "workspace_not_registered":
			message = remote.Message
		case code == transport.ErrCodeScopeRequired:
			message = "The pairing does not grant permission to run remote commands. Ask the user to reconnect with full device access."
		case code == transport.ErrCodeAuthFailed:
			message = "The destination refused this pairing's credentials. Reconnect the computer in Remote access."
		default:
			message = "The destination could not complete the request. Its reported error: " + remote.Message + ". If it is running an older AO build, update it for detailed command errors."
			uncertain = true
		}
	} else if !known {
		uncertain = !attachedbackends.NotSent(err)
		code, message = remoteFailureText(err, uncertain)
		cid := uuid.NewString()
		log.Printf("remote %s failed (id: %s): %v", action, cid, err)
		message += " Reference: " + cid + "."
	}
	return code, message, uncertain
}

// remoteFailureText is the code and message for a failure with no reviewed
// text: a connection, a cancellation or a timeout, else an internal error.
// sent says whether the destination may have the call, which is what the
// person or model must check before trying again.
func remoteFailureText(err error, sent bool) (code, message string) {
	var network net.Error
	switch {
	case errors.Is(err, context.Canceled) && sent:
		return "remote_request_canceled", "The request was canceled before a response was received. This does not cancel an accepted remote job."
	case errors.Is(err, context.Canceled):
		return "remote_request_canceled", "The request was canceled before it was sent."
	case errors.Is(err, context.DeadlineExceeded) && sent:
		return "remote_request_timeout", "Timed out waiting for the destination. Check its connection and try reading the job status again."
	case errors.Is(err, context.DeadlineExceeded):
		return "remote_request_timeout", "Timed out reaching the destination, so nothing was sent. Check its connection."
	case !errors.As(err, &network) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
		if sent {
			return "remote_internal_error", "Agent Overflow could not complete this request. Check the originating computer's logs using the reference below."
		}
		return "remote_internal_error", "Agent Overflow could not reach the destination, so nothing was sent. Check the originating computer's logs using the reference below."
	case sent:
		return "remote_connection_failed", "Could not get a response from the destination. Check that it is awake, Agent Overflow is running, and LAN or Tailscale connectivity is available."
	}
	return "remote_connection_failed", "Could not reach the destination, so nothing was sent. Check that it is awake, Agent Overflow is running, and LAN or Tailscale connectivity is available."
}

// remoteIssueText is the message a row shows for a failed check on its job.
// The row names the computer and the job itself, so the text carries neither
// ids nor a code.
func remoteIssueText(action string, err error) string {
	_, message, _ := remoteErrorDetails(action, err)
	return message
}

// remoteUserError is the failure of something a person did from the UI, in
// their words: what could not be done to which job on which computer.
func (a *App) remoteUserError(verb, computerID, requestID string, err error) error {
	if err == nil {
		return nil
	}
	ref := a.remoteRef(computerID, requestID)
	code, message, _ := remoteErrorDetails(verb, err)
	subject := "the remote job"
	if ref.Label != "" {
		subject = strconv.Quote(ref.Label)
	}
	where := ""
	if ref.ComputerName != "" {
		where = " on " + ref.ComputerName
	}
	return errorsx.Public(code, fmt.Sprintf("Could not %s %s%s: %s", verb, subject, where, message), err)
}

func remoteErrorText(err error) string {
	if code, message, ok := errorsx.PublicDetails(err); ok {
		return "[" + code + "] " + message
	}
	return err.Error()
}

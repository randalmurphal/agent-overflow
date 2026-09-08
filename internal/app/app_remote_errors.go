package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

// Never infer acceptance from an RPC failure. The chosen request ID is the
// recovery handle even when the destination ran the command but lost its reply.
func remoteOperationError(action, computerID, requestID string, err error) error {
	if err == nil {
		return nil
	}
	code, message, known := errorsx.PublicDetails(err)
	uncertain := false
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
		code = "remote_internal_error"
		message = "Agent Overflow could not complete this request. Check the originating computer's logs using the reference below."
		var network net.Error
		if errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			code = "remote_connection_failed"
			message = "Could not get a response from the destination. Check that it is awake, Agent Overflow is running, and LAN or Tailscale connectivity is available."
		}
		if errors.Is(err, context.Canceled) {
			code = "remote_request_canceled"
			message = "The request was canceled before a response was received. This does not cancel an accepted remote job."
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = "remote_request_timeout"
			message = "Timed out waiting for the destination. Check its connection and try reading the job status again."
		}
		cid := uuid.NewString()
		log.Printf("remote %s failed (id: %s): %v", action, cid, err)
		message += " Reference: " + cid + "."
		uncertain = true
	}
	prefix := "Remote " + action
	if entityid.Valid(computerID) {
		prefix += " on computer " + computerID
	}
	if entityid.Valid(requestID) {
		prefix += " for request " + requestID
	}
	if uncertain && entityid.Valid(requestID) {
		message += " Check remote_status with these same IDs."
		if action == "run" {
			message += " If necessary, retry identical arguments with the SAME request_id; a new ID could run the command twice."
		}
		if action == "cancel" {
			message += " Cancellation is not confirmed; if the job is still running, retry remote_cancel with these IDs."
		}
	}
	return errorsx.Public(code, fmt.Sprintf("%s: %s", prefix, message), err)
}

func remoteErrorText(err error) string {
	if code, message, ok := errorsx.PublicDetails(err); ok {
		return "[" + code + "] " + message
	}
	return err.Error()
}

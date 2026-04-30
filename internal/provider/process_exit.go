package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// ProcessExitInfo describes an unexpected provider subprocess exit.
//
// Reason is rendered verbatim into chat-visible summaries and the
// provider:session_died banner. Today MarshalProcessExitMeta populates
// it from controlled Go-stdlib formats (`*exec.ExitError`, signal
// names) — DO NOT pass raw stderr or any user-controlled text into
// this field. A future caller that wraps stderr in here would let a
// hostile provider binary inject text into the chat UI.
type ProcessExitInfo struct {
	Reason   string `json:"reason"`
	ExitCode int    `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

// MarshalProcessExitMeta converts a subprocess exit error into event metadata.
func MarshalProcessExitMeta(err error) json.RawMessage {
	info := ProcessExitInfo{Reason: "provider process exited unexpectedly"}
	if err != nil {
		info.Reason = err.Error()
	}

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			switch {
			case status.Signaled():
				info.Signal = status.Signal().String()
				info.Reason = fmt.Sprintf("provider process terminated by signal %s", info.Signal)
			case status.Exited():
				info.ExitCode = status.ExitStatus()
				info.Reason = fmt.Sprintf("provider process exited with code %d", info.ExitCode)
			}
		}
	}

	data, marshalErr := json.Marshal(info)
	if marshalErr != nil {
		return nil
	}
	return data
}

// WaitProcessExitErr waits briefly for the process waiter to publish its final
// exit error, then returns the most recent value.
func WaitProcessExitErr(proc *Process) error {
	select {
	case <-proc.Done():
	case <-time.After(100 * time.Millisecond):
	}
	return proc.Err()
}

package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
//
// StderrTail is the provider-controlled channel for the process's
// last stderr output — it exists so an exit-1-at-startup (bad flag,
// missing module) is diagnosable from the UI instead of only from a
// host-side log. It is chat-visible too, so it must only ever be
// populated through SanitizeChildStderr (single line, hard length
// cap); MarshalProcessExitMeta enforces that.
type ProcessExitInfo struct {
	Reason     string `json:"reason"`
	ExitCode   int    `json:"exitCode,omitempty"`
	Signal     string `json:"signal,omitempty"`
	StderrTail string `json:"stderrTail,omitempty"`
}

// SanitizeChildStderr bounds child-process stderr before it lands in
// a user-facing message. Provider CLIs inherit AO's full os.Environ()
// (intentionally — env vars are how MCP bearer-token indirection is
// resolved), so a CLI panic that dumped process.env would otherwise
// channel a token through to the UI verbatim. 256B is enough for
// typical CLI errors ("command not found", "unknown option ...",
// "config invalid: ...") while keeping a runaway dump off the wire.
// Newlines collapse so the text renders on one line.
func SanitizeChildStderr(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	const cap = 256
	if len(s) > cap {
		return s[:cap] + "…(truncated)"
	}
	return s
}

// MarshalProcessExitMeta converts a subprocess exit error into event metadata.
//
// Non-ExitError errors (fork/exec failures, startup errors) get a
// generic reason because err.Error() can contain host filesystem
// paths. The raw error is logged server-side by the caller.
//
// stderrTail is the raw captured tail from Process.StderrTail (empty
// when the caller has none); it is sanitized here — never rely on the
// caller having done it.
func MarshalProcessExitMeta(err error, stderrTail string) json.RawMessage {
	info := ProcessExitInfo{
		Reason:     "provider process exited unexpectedly",
		StderrTail: SanitizeChildStderr(stderrTail),
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
	} else if err != nil {
		info.Reason = "provider failed to start"
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

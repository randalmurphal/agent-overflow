package app

import (
	"errors"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/threadtools"
)

// threadCleanupError is the cleanup after a committed archive or delete that
// did not finish. Each part field is the cause of one part that failed. Its
// public message says what happened to the thread and what may still be
// running or open, so no caller reads it as an archive or delete that did
// not happen.
type threadCleanupError struct {
	// outcome is what happened to the thread: "archived" or "deleted".
	outcome string
	// session is set when the provider process may still be running: its
	// turn history could not be read, or its close failed.
	session error
	// background is set when the provider closed but its background calls
	// could not be marked ended (errBackgroundNotRetired).
	background error
	browser    error
	// asks is set when an ask the thread owns could not be stopped.
	asks error
	// commands is set when a cancel did not reach the computer running the
	// command, which keeps it until its owner grace ends it
	// (remotejobs.DefaultOwnerGrace).
	commands error
	// commandRecords is set when the thread's remote command records could
	// not be read or dismissed.
	commandRecords error
}

// parts pairs each failed part with what the person is told about it, in
// message order.
func (e *threadCleanupError) parts() []cleanupPart {
	return []cleanupPart{
		{e.session, "its provider session could not be closed, so it and the processes it started may still be running"},
		{e.background, "its background tasks ended with its session but may still show as running"},
		{e.browser, "its browser pages could not all be closed"},
		{e.asks, "an ask it sent to another thread could not be stopped and may keep running"},
		{e.commands, "a command it started on another computer could not be cancelled and may keep running there for up to an hour"},
		{e.commandRecords, "its commands on other computers may not all have been cancelled"},
	}
}

type cleanupPart struct {
	cause error
	says  string
}

// err is the cleanup's failure, or nil when every part succeeded. The
// public message is the outermost layer, so it is what every caller is
// shown even when a cause carries public prose of its own.
func (e threadCleanupError) err() error {
	for _, part := range e.parts() {
		if part.cause != nil {
			return errorsx.Public(threadtools.CodeCleanupFailed, e.message(), &e)
		}
	}
	return nil
}

func (e *threadCleanupError) Error() string { return e.Unwrap().Error() }

func (e *threadCleanupError) Unwrap() error {
	var causes []error
	for _, part := range e.parts() {
		causes = append(causes, part.cause)
	}
	return errors.Join(causes...)
}

func (e *threadCleanupError) message() string {
	var says []string
	for _, part := range e.parts() {
		if part.cause != nil {
			says = append(says, part.says)
		}
	}
	if last := len(says) - 1; last > 0 {
		says[last] = "and " + says[last]
	}
	return "The thread was " + e.outcome + ", but " + strings.Join(says, ", ") + "."
}

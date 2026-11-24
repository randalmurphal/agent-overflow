package app

import (
	"errors"
	"strings"
)

// threadCleanupError is the cleanup after a committed archive or delete that
// did not finish. Each part field is the cause of one part that failed. Its
// message says what happened to the thread and what may still be running or
// open, so no caller reads it as an archive or delete that did not happen.
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
	// schedule is set when the thread's loops, watches or reminders could
	// not all be cancelled.
	schedule error
	// queue is set when what the app queued for the thread could not all be
	// read or withdrawn.
	queue error
	// workers is set when an archive could not read the sub-threads it goes
	// on to archive.
	workers error
	// remoteWorkers is set when an archive could not record the archives it
	// owes the sub-threads on paired computers.
	remoteWorkers error
	// below gathers the parts that failed for the threads under it that the
	// archive archived.
	below *threadCleanupError
}

// parts pairs each failed part with what the person is told about it, in
// message order.
func (e *threadCleanupError) parts() []cleanupPart {
	return []cleanupPart{
		{&e.session, "its provider session could not be closed, so it and the processes it started may still be running"},
		{&e.background, "its background tasks ended with its session but may still show as running"},
		{&e.browser, "its browser pages could not all be closed"},
		{&e.asks, "an ask it sent to another thread could not be stopped and may keep running"},
		{&e.commands, "a command it started on another computer could not be cancelled and may keep running there for up to an hour"},
		{&e.commandRecords, "its commands on other computers may not all have been cancelled"},
		{&e.schedule, "its loops, watches and reminders may not all have been cancelled"},
		{&e.queue, "messages queued for it may not all have been withdrawn"},
		{&e.workers, "its sub-threads could not be read, so some may still be open; archiving it again archives them"},
		{&e.remoteWorkers, "its sub-threads on other computers were not sent the archive; archiving it again sends it"},
	}
}

type cleanupPart struct {
	cause *error
	says  string
}

// addBelow gathers what a thread under this one could not clean up, and
// what the threads under that one could not.
func (e *threadCleanupError) addBelow(child threadCleanupError) {
	if child.Unwrap() == nil {
		return
	}
	if e.below == nil {
		e.below = &threadCleanupError{outcome: e.outcome}
	}
	mine := e.below.parts()
	for i, part := range child.parts() {
		*mine[i].cause = errors.Join(*mine[i].cause, *part.cause)
	}
	if child.below != nil {
		e.addBelow(*child.below)
	}
}

func (e *threadCleanupError) Error() string { return e.Unwrap().Error() }

// Unwrap is every part's cause, the threads below included, or nil.
func (e *threadCleanupError) Unwrap() error {
	var causes []error
	for _, part := range e.parts() {
		causes = append(causes, *part.cause)
	}
	if e.below != nil {
		causes = append(causes, e.below.Unwrap())
	}
	return errors.Join(causes...)
}

// message says what happened to the thread and what may still be running or
// open, here and under it; empty when nothing failed.
func (e *threadCleanupError) message() string {
	if e.Unwrap() == nil {
		return ""
	}
	text := "The thread was " + e.outcome
	if says := e.says(); says != "" {
		text += ", but " + says
	}
	text += "."
	if e.below != nil && e.below.Unwrap() != nil {
		text += " A thread under it was " + e.outcome + " too, but " + e.below.says() + "."
	}
	return text
}

// says lists this thread's failed parts, empty when none failed.
func (e *threadCleanupError) says() string {
	var says []string
	for _, part := range e.parts() {
		if *part.cause != nil {
			says = append(says, part.says)
		}
	}
	if last := len(says) - 1; last > 0 {
		says[last] = "and " + says[last]
	}
	return strings.Join(says, ", ")
}

// Package procutil holds the two things every supervised child process this
// app starts needs: a process-group kill configuration so a cancelled command
// cannot leave its own children running, and a bounded output tail so an
// unbounded stream is never buffered whole.
package procutil

// Package procutil holds what supervised child processes this app starts
// need: a process-group kill configuration so a cancelled command cannot
// leave its own children running, a bounded output tail so an unbounded
// stream is never buffered whole, and a run that delivers a command's
// output whole even when a descendant keeps its pipes open.
package procutil

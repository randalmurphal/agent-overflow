package triage

// processBoundState is the thread state that belongs to one provider
// process: what a frame of a retired session must not write. It is the
// single list of that state. Every writer of an entry takes the frame's
// session token and refuses a retired one under the r.mu it writes under
// (frameRetiredLocked). MarkThreadActive runs each entry's clear when it
// commits the thread to a new session. An entry without a clear is not
// reset by the commit: the context-repair restart reaches MarkThreadActive
// without CleanupThread and keeps it live, and its own lifecycle or
// CleanupThread ends it.
//
// TestProcessStateWritersRefuseARetiredSessionsFrame is keyed by these
// names and fails when an entry has no refusal case, so state added here
// cannot go unguarded.
var processBoundState = []processBoundEntry{
	// A stale settlement marker would misroute a replacement session's
	// first error result into persistLateTurnPayload (see MarkThreadActive).
	{name: "turn settlement", clear: stateClear(func(st *threadState) { st.settledTurns = nil })},
	{name: "effective model", clear: stateClear(func(st *threadState) { st.effectiveModel, st.effectiveModelSet = "", false })},
	// A pending wakeup and armed cron jobs live in the previous CLI
	// process. A stale deadline would shield the replacement from the
	// idle reaper.
	{name: "schedule", clear: stateClear((*threadState).clearProcessSchedule)},
	// A dead process never delivers the terminal that would consume its
	// ticks.
	{name: "subagent progress", clear: stateClear(func(st *threadState) { st.subagentProgress = nil })},
	// A replacement process is never mid-compaction.
	{name: "compacting window", clear: stateClear(func(st *threadState) { st.compactingSince, st.compactingSinceSet = 0, false })},
	// The acks the correlation waits on can only come from the dead
	// process.
	{name: "command lifecycle", clear: stateClear(func(st *threadState) { st.commandLifecycle = nil })},
	// An in-flight echo cannot survive the session whose read loop carries
	// it, and a replacement after revert reuses turn indexes, so a
	// lingering mark would settle a reused-index predecessor
	// "interrupted". Entries are keyed by router-unique seqs, so a stale
	// restore's removal against the fresh list finds nothing.
	{name: "interrupt marks", hostWritten: true, clear: func(_ *threadState, id *threadIdentity) { id.interruptMarks = nil }},
	{name: "open turn and round"},
	{name: "streams"},
	{name: "approvals"},
	{name: "user inputs"},
	{name: "codex live tray"},
}

type processBoundEntry struct {
	name string
	// clear resets the entry when a session commits. st is nil when the
	// thread has no state entry. Runs under r.mu.
	clear func(st *threadState, id *threadIdentity)
	// hostWritten is state only host calls write. They carry no session
	// token, so there is no frame writer to refuse.
	hostWritten bool
}

func stateClear(clear func(*threadState)) func(*threadState, *threadIdentity) {
	return func(st *threadState, _ *threadIdentity) {
		if st != nil {
			clear(st)
		}
	}
}

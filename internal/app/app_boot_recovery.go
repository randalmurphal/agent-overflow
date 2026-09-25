package app

import "log"

// settlePriorInstance settles what the previous app instance left in
// flight, one boot phase per sweep. It runs before any provider session
// can spawn, so everything a sweep finds is the previous instance's. A
// sweep that fails is reported on its phase (bootPhaseFailed), which every
// client shows, and the boot goes on; the next start runs it again. See
// docs/architecture/turn-lifecycle.md §Crash behavior.
func (a *App) settlePriorInstance() {
	// An in-app session death settles its turn through the synthesized
	// truncated turn-complete. An app crash leaves completed_at NULL, which
	// GetActiveTurn reads as a live turn: revert waits on an interrupt with
	// nothing to interrupt, and the turn's streaming items never settle.
	endPhase := a.bootPhase("app.recover_crashed_turns", "Settling interrupted turns")
	if settled, err := a.triage.RecoverCrashedTurns(); err != nil {
		a.bootPhaseFailed(err)
	} else if settled > 0 {
		log.Printf("app: settled %d crashed in-flight turns as interrupted", settled)
	}
	endPhase()
	// Codex child identities are resumable, but live turns and background
	// PTYs belong to the app-server process. Retiring them keeps the tray
	// from presenting prior-process work as still running.
	endPhase = a.bootPhase("app.recover_codex_background_runtime", "Settling background agents")
	if err := a.recoverCodexBackgroundRuntimeOnStartup(); err != nil {
		a.bootPhaseFailed(err)
	}
	endPhase()
	// Background launches whose Claude session did not survive get a
	// session_died completion; no live agent will ever report them.
	endPhase = a.bootPhase("app.recover_orphaned_background_tasks", "Settling background tasks")
	if recovered, err := a.triage.RecoverOrphanedBackgroundTasks(); err != nil {
		a.bootPhaseFailed(err)
	} else if recovered > 0 {
		log.Printf("app: recovered %d Claude background launches as session_died", recovered)
	}
	endPhase()
	// A worktree setup runs only inside a live process, so a 'running' row
	// is residue over a worktree whose state nobody can vouch for: 'failed',
	// which puts the retry in reach.
	endPhase = a.bootPhase("app.sweep_crashed_worktree_setups", "Settling worktree setups")
	if err := a.sweepCrashedWorktreeSetups(); err != nil {
		a.bootPhaseFailed(err)
	}
	endPhase()
}

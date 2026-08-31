package app

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/sessionimport"
	"agent-overflow/internal/store"
)

// materializeImportedClaudeBranch keeps inactive Claude branches imported by
// older Agent Overflow releases continuable. Current imports create only the
// active provider-session thread, which already carries its resume reference.
//
// WHY THE LEGACY THREAD HAS NONE. Releases that imported one thread per DAG
// leaf could give the source session id only to the active branch:
// `claude --resume <id>` reopens that chain and nothing else. Inactive threads
// therefore stored their source leaf in thread_import_state with no ref.
//
// WHY IT HAPPENS HERE AND NOT AT IMPORT. Cutting a branch out costs a new
// transcript file on disk, and that file shows up in the user's own
// `claude --resume` picker. The old importer deliberately deferred that write
// rather than create hundreds of phantom provider sessions and duplicate
// transcript prefixes for threads the user might never continue. Doing it on
// first session start costs the same write for
// legacy branches that are actually used, and nothing for the rest.
//
// WHAT IT WRITES. sessionfork.WriteForkFileThroughUUID keeps everything
// through the branch's leaf in file order and remints every uuid, which is the
// same transform (and the same resulting shape: the leaf is the new file's
// last transcript row, so it is the new file's active branch) that
// fork-at-a-past-message has produced in `app_thread_fork.go` since forking
// existed. Rows of other branches that happen to sit earlier in the file come
// along off-chain, exactly as they do for a fork. It carries no `<sessionID>/`
// subagent sidecar — no fork ever has, and the cut's session id is new, so
// there is none to carry in either destination. Because the cut remints UUIDs,
// the session-ref move and SQLite's provider-id correlations commit together;
// otherwise a later rollback or fork would search the new file for old UUIDs.
//
// WHERE IT WRITES. Under the slug of the thread's CURRENT workspace, not
// beside the source. Claude resolves `--resume` against the slug of the cwd it
// is launched in, and this thread has no session_ref yet — which is exactly
// what makes a workspace change before the first send a silent no-op
// (`copyClaudeSessionForWorkspaceChange` has no transcript to relocate). Cut
// beside the source, the file would land under the ORIGINAL cwd's slug while
// the resume looks under the new one, and the first send would brick with "No
// conversation found". When the destination slug is unresolvable the cut still
// happens beside the source: that is the behaviour that shipped, and it is
// never worse than not writing at all.
//
// FAILURE IS A DEGRADE, NOT A REFUSAL. When the source transcript is gone or
// the leaf is no longer in it, the thread starts a fresh provider session —
// which is precisely the behaviour it had before this function existed, and
// the timeline the user sees is unaffected because AO's own copy of the
// history is in SQLite. Refusing the start instead would make an imported
// branch unusable because a file it no longer needs was deleted. The failure
// is logged with the leaf and the path so it can be told apart from a thread
// that never had a ref to begin with.
func (a *App) materializeImportedClaudeBranch(t store.Thread) store.Thread {
	if a.store == nil ||
		t.Provider != string(provider.Claude) ||
		t.ImportSource != sessionimport.ProviderClaude ||
		strings.TrimSpace(t.SessionRef) != "" ||
		strings.TrimSpace(t.PendingForkRef) != "" {
		return t
	}

	state, found, err := a.store.GetThreadImportState(t.ID)
	if err != nil {
		log.Printf("start session: read import state for thread %s: %v", t.ID, err)
		return t
	}
	if !found || strings.TrimSpace(state.LeafUUID) == "" || strings.TrimSpace(state.SourcePath) == "" {
		return t
	}
	if _, err := os.Stat(state.SourcePath); err != nil {
		log.Printf(
			"start session: imported branch %s cannot be resumed — its source transcript %s is gone; starting a fresh session",
			t.ID, state.SourcePath)
		return t
	}

	sessionID, path, uuidMap, err := sessionfork.WriteForkFileThroughUUID(sessionfork.ForkCut{
		SourcePath:   state.SourcePath,
		DestDir:      importedBranchDestDir(t, state.SourcePath),
		LastKeptUUID: state.LeafUUID,
		Title:        t.Title,
	})
	if err != nil {
		log.Printf(
			"start session: imported branch %s could not be cut from %s at leaf %s; starting a fresh session: %v",
			t.ID, state.SourcePath, state.LeafUUID, err)
		return t
	}
	itemUpdates, anchorUpdates, err := a.computeClaudeProviderIDRemap(t.ID, uuidMap)
	if err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			log.Printf("start session: remove orphaned branch session %s: %v", path, removeErr)
		}
		log.Printf("start session: compute imported branch id remap for thread %s: %v", t.ID, err)
		return t
	}

	// A TARGETED write, not a whole-row UpdateThread. `t` was read at the top
	// of startSessionNowWithClaudeResumeAt and everything else about the row is
	// unread here; writing it back would revert any column another writer moved
	// since — an auto-generated title (which lands from a detached goroutine
	// through its own compare-and-swap), an observed branch, a token-usage
	// refresh. UpdateSessionRefAndRemapProviderIDs writes session_ref, clears
	// the pending fork ref, remaps correlations in the same transaction, and
	// leaves updated_at alone.
	if _, err := a.store.UpdateSessionRefAndRemapProviderIDs(
		t.ID, sessionID, itemUpdates, anchorUpdates,
	); err != nil {
		// The file is written but the row does not point at it. Remove it
		// rather than leave an orphan transcript in the user's Claude home;
		// the next start tries again from the same source.
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			log.Printf("start session: remove orphaned branch session %s: %v", path, removeErr)
		}
		log.Printf("start session: persist imported branch session for thread %s: %v", t.ID, err)
		return t
	}
	t.SessionRef = sessionID
	return t
}

// importedBranchDestDir resolves the Claude project directory a thread's cut
// branch must land in: the slug of the thread's CURRENT workspace, under the
// same Claude home the source transcript was read from.
//
// That home comes from sourcePath (`<projectsDir>/<slug>/<id>.jsonl`, recorded
// by the import out of the injected provider home) rather than from $HOME.
// The two agree in production and diverge under AO_HARNESS_KEEP_HOME, where
// the app's Claude home is `<dataRoot>/home/.claude` while $HOME is the
// developer's own — and a cut resolved from $HOME would write into the real
// `~/.claude/projects`, which the harness's read-only-widening property
// forbids. Deriving it from the source makes the destination structurally
// unable to leave the home the transcript came from.
//
// Returns "" — sessionfork's "beside the source" — for two cases. The
// resolution FAILURE is logged so a resume that later fails is diagnosable: the
// workspace cannot be canonicalized, i.e. the directory is gone (a removed
// worktree the thread has not been reattached from yet). Path LENGTH is no
// longer such a case — the CLI's truncate-and-hash slug is reproduced exactly.
//
// The other — the thread records no workspace — is silent: there is nothing to
// resolve, which is not a failure, and every workspace-less thread logging a
// line about it would be noise.
//
// Beside the source is right on its own merits whenever the workspace never
// moved: the source transcript is already under that workspace's slug, so the
// resolved directory and the source directory are the same path.
func importedBranchDestDir(t store.Thread, sourcePath string) string {
	workspace := strings.TrimSpace(t.WorkspacePath)
	if workspace == "" {
		return ""
	}
	// `<projectsDir>/<slug>/<sessionID>.jsonl` — up two, to the projects dir.
	projectsDir := filepath.Dir(filepath.Dir(sourcePath))
	dir, err := sessionfork.WorkspaceProjectDir(projectsDir, workspace)
	if err != nil {
		log.Printf(
			"start session: imported branch %s could not resolve the project dir of workspace %s; cutting beside the source transcript: %v",
			t.ID, workspace, err)
		return ""
	}
	return dir
}

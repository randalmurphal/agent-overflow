package triage

import "agent-overflow/internal/store"

// ProjectUpdateEvent is the wire shape for project:updated. Every persisted
// project-row mutation broadcasts one, so a second attached client converges
// on the change without a refresh; a write that changed nothing broadcasts
// nothing.
//
// The vocabulary is ThreadUpdateEvent's, for the same reason and with the same
// meanings: Action names what the receiver must DO with the row, not which RPC
// ran, because membership of the sidebar list is not derivable from the row
// alone. The list the client holds is the non-archived projects, so archiving
// removes a row that still exists — a distinction "the row changed" cannot
// carry.
//
//   - "full"     the row's current state — converge a row the client already
//     has. Says nothing about membership, so a client that does not have the
//     row ignores it. Rename, reorder, and worktree-setup writes use it.
//   - "listed"   the row belongs in the sidebar now — insert it if absent
//     (create, unarchive, and the implicit create that happens when a thread
//     is started in a workspace no project covers yet).
//   - "unlisted" the row has left the sidebar but still exists (archive).
//     Carries the row so a client displaying it converges before dropping it.
//   - "deleted"  the row is gone from SQLite. ID only.
//
// This type lives beside ThreadUpdateEvent deliberately: the two vocabularies
// have to stay the same one, and splitting them across packages is how they
// drift.
type ProjectUpdateEvent struct {
	Action  string         `json:"action"`
	Project *store.Project `json:"project,omitempty"`
	ID      string         `json:"id,omitempty"`
}

// The Action vocabulary, spelled once so an emit site cannot invent a value
// the frontend's applier does not handle.
const (
	ProjectActionFull     = "full"
	ProjectActionListed   = "listed"
	ProjectActionUnlisted = "unlisted"
	ProjectActionDeleted  = "deleted"
)

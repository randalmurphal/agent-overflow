package app

import (
	"context"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// Where a thread came from: the screen that started it and the git coordinates
// of its workspace at that moment.
//
// Both are write-once and both are observed HERE, at the one moment they are
// still true. A thread's branch moves, its head commit gets rebased away, and
// its workspace may end up holding something else entirely; asking later
// produces a confident wrong answer rather than an empty one. That matters
// because the consumer is fork-and-transfer, which has to reconstruct where a
// thread grew from on a machine that never had the workspace.
//
// Every field is optional. A workspace outside a repository, a detached HEAD,
// a repository with no remote, an in-process call with no screen behind it —
// all of these produce empty values, and empty means "not known". Nothing here
// fails, because none of it is worth failing a thread creation over.

// observeThreadOrigin reads a workspace's git coordinates. Three subprocess
// reads, two of which the repo-metadata cache usually answers, run once per
// created thread — not on any hot path.
func (a *App) observeThreadOrigin(workspacePath string) store.ThreadOrigin {
	if workspacePath == "" {
		return store.ThreadOrigin{}
	}
	core := a.gitCore()
	if core == nil {
		return store.ThreadOrigin{}
	}
	// HeadSHA errors on an unborn branch and outside a repository. Both are
	// "not known", which is the zero value, so the error is dropped rather
	// than propagated: a thread in a freshly-inited repo must still be
	// creatable.
	head, _ := core.HeadSHA(workspacePath)
	return store.ThreadOrigin{
		Branch:     core.CurrentBranch(workspacePath),
		RemoteURL:  core.OriginRemoteURL(workspacePath),
		HeadCommit: head,
	}
}

// creatingDevice names the screen a thread-creating call came from. Empty for
// an in-process call, a background saga, or a test — a normal answer meaning
// "the backend did this itself", not an error.
//
// The DEVICE id, not the connection id: this outlives the page load that made
// it, which is the whole point of recording it.
func creatingDevice(ctx context.Context) string {
	return transport.ClientFromContext(ctx).DeviceID
}

// stampThreadCreation fills the write-once creation facts on a thread about to
// be inserted. One call site per creation path, so a path that forgets it
// produces empty provenance rather than wrong provenance.
func (a *App) stampThreadCreation(ctx context.Context, thread *store.Thread) {
	if thread == nil {
		return
	}
	thread.CreatedByDevice = creatingDevice(ctx)
	thread.Origin = a.observeThreadOrigin(thread.WorkspacePath)
}

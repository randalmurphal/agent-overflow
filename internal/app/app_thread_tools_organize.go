package app

import (
	"context"

	"agent-overflow/internal/threadtools"
)

// The organizing half of threadtools.App: thread_update and thread_group.
// Stubbed until the organize patch lands; see app_thread_tools_writes.go
// for the refusal these share.

func (t threadToolsApp) UpdateThreads(context.Context, threadtools.Caller, threadtools.UpdateCall) (threadtools.UpdateReport, error) {
	return threadtools.UpdateReport{}, threadToolsWriteUnavailable("organizing threads")
}

func (t threadToolsApp) UpdateGroup(context.Context, threadtools.Caller, threadtools.GroupCall) (threadtools.GroupReport, error) {
	return threadtools.GroupReport{}, threadToolsWriteUnavailable("organizing groups")
}


package app

import (
	"context"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/remotejobs"
)

type RemoteLogChunk = remotejobs.LogChunk
type RemoteLogSearch = remotejobs.LogSearch

//ao:scope terminal:operate
//ao:route selected
func (a *App) RemoteCommandReadLog(ctx context.Context, id string, offset int64, maxBytes int) (RemoteLogChunk, error) {
	if a.remoteJobs == nil {
		return RemoteLogChunk{}, errorsx.Public("remote_not_ready", "Remote commands are not ready on this computer.", nil)
	}
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return RemoteLogChunk{}, err
	}
	return a.remoteJobs.ReadLog(owner, id, offset, maxBytes)
}

//ao:scope terminal:operate
//ao:route selected
func (a *App) RemoteCommandSearchLog(ctx context.Context, id, query string, offset int64, maxBytes int) (RemoteLogSearch, error) {
	if a.remoteJobs == nil {
		return RemoteLogSearch{}, errorsx.Public("remote_not_ready", "Remote commands are not ready on this computer.", nil)
	}
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return RemoteLogSearch{}, err
	}
	return a.remoteJobs.SearchLog(owner, id, query, offset, maxBytes)
}

//ao:scope terminal:operate
func (a *App) ReadThreadRemoteLog(ctx context.Context, threadID, computerID, requestID string, offset int64, maxBytes int) (RemoteLogChunk, error) {
	var result RemoteLogChunk
	err := a.callThreadRemoteJob(ctx, threadID, computerID, requestID, "RemoteCommandReadLog", &result, requestID, offset, maxBytes)
	return result, err
}

// callThreadRemoteJob rechecks the destination receipt before any output read.
// A claimed thread ID or a local watch alone is not destination authority.
func (a *App) callThreadRemoteJob(ctx context.Context, threadID, computerID, requestID, method string, result any, args ...any) error {
	if a.backends == nil {
		return errNoBackendProfiles
	}
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var receipt RemoteCommand
	if err := a.backends.CallAgentPeer(call, computerID, "RemoteCommandStatus", &receipt, requestID); err != nil {
		return remoteOperationError("read", computerID, requestID, err)
	}
	if receipt.ID != requestID || receipt.SourceThreadID != threadID {
		return errorsx.Public("remote_wrong_conversation", "This command belongs to another conversation. Read it from the conversation that submitted it.", nil)
	}
	return remoteOperationError("read", computerID, requestID, a.backends.CallAgentPeer(call, computerID, method, result, args...))
}

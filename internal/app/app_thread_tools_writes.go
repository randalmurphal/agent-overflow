package app

import (
	"context"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/threadtools"
)

// The write half of threadtools.App is not built yet.
//
// Everything in this file spawns, sends to, answers, cancels, reminds or
// organizes threads, and all of it depends on the request ledger, the
// wake delivery and the attribution chip that the next phase brings. The
// read half above is complete and answers for real.
//
// One refusal serves them all: the model gets a public code it can act on
// and prose that says the capability is not here rather than that the call
// was malformed. Nothing here partially applies a write.
func threadToolsWriteUnavailable(what string) error {
	return errorsx.Public(threadtools.CodeInvalidRequest,
		"Thread tools can read this computer's threads but "+what+" is not available yet in this build.", nil)
}

func (t threadToolsApp) Spawn(context.Context, threadtools.Caller, threadtools.SpawnCall) (threadtools.RequestAck, error) {
	return threadtools.RequestAck{}, threadToolsWriteUnavailable("starting a thread")
}

func (t threadToolsApp) Send(context.Context, threadtools.Caller, threadtools.SendCall) (threadtools.RequestAck, error) {
	return threadtools.RequestAck{}, threadToolsWriteUnavailable("sending to a thread")
}

func (t threadToolsApp) Ask(context.Context, threadtools.Caller, threadtools.AskCall) (threadtools.RequestAck, error) {
	return threadtools.RequestAck{}, threadToolsWriteUnavailable("asking a thread")
}

func (t threadToolsApp) Reply(context.Context, threadtools.Caller, threadtools.ReplyCall) (threadtools.ReplyAck, error) {
	return threadtools.ReplyAck{}, threadToolsWriteUnavailable("replying to a request")
}

func (t threadToolsApp) RequestStates(context.Context, threadtools.Caller, threadtools.StatusCall) (threadtools.StatusReport, error) {
	return threadtools.StatusReport{}, threadToolsWriteUnavailable("request status")
}

func (t threadToolsApp) ListRequests(context.Context, threadtools.Caller, threadtools.ListCall) (threadtools.RequestListing, error) {
	return threadtools.RequestListing{}, threadToolsWriteUnavailable("request status")
}

func (t threadToolsApp) Cancel(context.Context, threadtools.Caller, threadtools.CancelCall) (threadtools.CancelReport, error) {
	return threadtools.CancelReport{}, threadToolsWriteUnavailable("cancelling a request")
}

func (t threadToolsApp) Remind(context.Context, threadtools.Caller, threadtools.RemindCall) (threadtools.RequestAck, error) {
	return threadtools.RequestAck{}, threadToolsWriteUnavailable("reminders")
}

func (t threadToolsApp) ExportAnswer(context.Context, threadtools.Caller, string) (threadtools.ExportFile, error) {
	return threadtools.ExportFile{}, threadToolsWriteUnavailable("request status")
}

package app

import (
	"context"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// `/side-chat`: the person's ephemeral fork, and the Keep action that turns
// one into an ordinary thread.
//
// The companion pane owns a side chat's lifetime. Closing it deletes the
// thread through DeleteThread, which drops the scratch record with the row,
// and the boot sweep deletes whatever a restart left behind.

// sideChatNotScratchCode is what a Keep on a thread that is not a side chat
// answers with. The pane only offers Keep on a scratch thread, so this is a
// stale pane or a second Keep.
const sideChatNotScratchCode = "thread_not_scratch"

// ForkSideChat forks a thread at its tail into a hidden scratch thread and
// records where it came from, so the fork can be kept or deleted later.
//
// The fork inherits the source's runtime mode, unlike an agent's
// `thread_ask` fork: a person is present in this pane continuing their own
// conversation, and a side chat that could not act where its source can
// would not be the conversation they forked.
//
//ao:scope threads:operate
func (a *App) ForkSideChat(ctx context.Context, threadID string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("side chat: store unavailable")
	}
	source, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("side chat: %w", err)
	}
	// Runtime mode is left empty so the fork keeps the source's, which is
	// what the doc comment above promises.
	return a.forkScratchThread(ctx, source, scratchForkOptions{TitlePrefix: sideChatTitlePrefix})
}

// sideChatTitlePrefix and askScratchTitlePrefix label the two kinds of
// scratch fork wherever a person can still see one: a side-chat pane's
// header, and the sidebar a kept fork lands in.
const (
	sideChatTitlePrefix   = "Side chat"
	askScratchTitlePrefix = "Ask"
)

// scratchForkOptions is what distinguishes the two scratch forks. Everything
// else about them is the same, which is why they share one creator.
type scratchForkOptions struct {
	// TitlePrefix names the kind of fork this is.
	TitlePrefix string
	// RuntimeMode is the fork's, empty to inherit the source's.
	RuntimeMode string
	// RequestToken is the agent request an ask fork answers. A side chat has
	// none: a person is driving it.
	RequestToken string
}

// forkScratchThread forks source at its tail into a hidden scratch thread and
// records where it came from, so the fork can be kept, deleted or swept.
//
// Both creators come through here, `/side-chat` and an agent's thread_ask,
// because the pair a scratch thread is, the fork row and the scratch record,
// has to be written together or neither.
func (a *App) forkScratchThread(
	ctx context.Context, source store.Thread, opts scratchForkOptions,
) (store.Thread, error) {
	fork, err := a.forkThreadTail(ctx, source.ID, forkOptions{
		Mode:        threadmode.ModeScratch,
		RuntimeMode: opts.RuntimeMode,
		Title:       scratchForkTitle(opts.TitlePrefix, source.Title),
	})
	if err != nil {
		return store.Thread{}, err
	}
	if err := a.store.InsertScratchThread(store.ScratchThread{
		ThreadID:       fork.ID,
		SourceThreadID: source.ID,
		ReturnMode:     scratchReturnMode(source.Mode),
		RequestToken:   opts.RequestToken,
	}); err != nil {
		// The fork exists and nothing owns it yet: no pane has opened on it
		// and no sweep knows about it. Take it back rather than leave a
		// hidden thread nothing can reach.
		if deleteErr := a.DeleteThread(fork.ID); deleteErr != nil {
			log.Printf("side chat: delete orphaned scratch fork %s: %v", fork.ID, deleteErr)
		}
		return store.Thread{}, err
	}
	return fork, nil
}

// scratchReturnMode is the mode a Keep promotion returns a scratch fork to,
// for both of its creators. The table's CHECK refuses `scratch` itself, and a
// hidden workflow mode is not something a person can keep, so both fall back
// to chat, which is what a promoted side conversation actually is.
func scratchReturnMode(sourceMode string) string {
	switch sourceMode {
	case threadmode.ModeChat, threadmode.ModePlan:
		return sourceMode
	default:
		return threadmode.ModeChat
	}
}

// PromoteScratchThread is Keep: the scratch thread takes back the mode
// recorded for it at the fork, which puts it in the sidebar, and its scratch
// record goes. The row is broadcast as listed so every attached client shows
// it, exactly as a fork is.
//
// A thread with no scratch record is refused: it is already an ordinary
// thread, and moving its mode here would be a mode change nobody asked for.
//
//ao:scope threads:operate
func (a *App) PromoteScratchThread(ctx context.Context, threadID string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("promote scratch thread: store unavailable")
	}
	unlock, err := a.threadApplication().LockMutable(ctx, threadID)
	if err != nil {
		return store.Thread{}, err
	}
	defer unlock()
	_, found, err := a.store.GetScratchThread(threadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("promote scratch thread: %w", err)
	}
	if !found {
		return store.Thread{}, errorsx.Public(sideChatNotScratchCode,
			"This thread is not a side chat, so there is nothing to keep.", nil)
	}
	promoted, err := a.store.PromoteScratchThread(threadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("promote scratch thread: %w", err)
	}
	a.broadcastThreadRow(triage.ThreadActionListed, promoted)
	return promoted, nil
}

// scratchForkTitle names a hidden fork after the thread it came from, with a
// fallback for a source that has not been titled yet.
func scratchForkTitle(prefix, sourceTitle string) string {
	title := strings.TrimSpace(sourceTitle)
	if title == "" {
		title = "thread"
	}
	return prefix + ": " + title
}

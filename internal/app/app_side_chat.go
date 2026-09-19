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
	fork, err := a.forkThreadTail(ctx, source.ID, forkOptions{
		Mode:  threadmode.ModeScratch,
		Title: sideChatTitle(source.Title),
	})
	if err != nil {
		return store.Thread{}, err
	}
	if err := a.store.InsertScratchThread(store.ScratchThread{
		ThreadID:       fork.ID,
		SourceThreadID: source.ID,
		ReturnMode:     scratchReturnMode(source.Mode),
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

func sideChatTitle(sourceTitle string) string {
	title := strings.TrimSpace(sourceTitle)
	if title == "" {
		title = "thread"
	}
	return "Side chat: " + title
}

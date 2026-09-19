package app

import (
	"context"
	"errors"
	"log"
	"sync"
)

// threadSearchIndexBuild owns the boot-time walk that fills the thread
// search index. The build is resumable and yields between batches, so the
// only lifecycle it needs is a goroutine this app cancels and joins.
type threadSearchIndexBuild struct {
	once sync.Once
	wg   sync.WaitGroup
}

// startThreadSearchIndex builds the search index in the background.
//
// thread_search answers from the index, and an index that has never been
// built answers nothing, so the build runs on every boot: it returns at
// once when the index is complete, and resumes from its committed cursor
// when it is not. Callers see the unfinished state through SearchIndexing,
// which is what marks a result partial rather than wrong.
func (a *App) startThreadSearchIndex() {
	if a.store == nil {
		return
	}
	a.threadSearchIndex.once.Do(func() {
		a.threadSearchIndex.wg.Add(1)
		go func() {
			defer a.threadSearchIndex.wg.Done()
			ctx := a.lifeCtx()
			if err := a.store.BuildSearchIndex(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("thread search: build index: %v", err)
			}
		}()
	})
}

// waitThreadSearchIndex joins the build. Shutdown cancels the app context
// first, and the build checks it between batches.
func (a *App) waitThreadSearchIndex() { a.threadSearchIndex.wg.Wait() }

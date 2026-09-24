package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func firstReadsSettled(g *firstReadsGate) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.settled
}

// answerRead serves one catalog read through the gate with the given result.
func answerRead(g *firstReadsGate, kind uint8, result error) {
	err := result
	g.read(kind)(&err)
}

func TestFirstReadsGateSettlesWhenBothCatalogsHaveAnswered(t *testing.T) {
	var g firstReadsGate
	answerRead(&g, firstReadThreads, nil)
	answerRead(&g, firstReadThreads, nil)
	if firstReadsSettled(&g) {
		t.Fatal("settled on thread reads alone")
	}
	answerRead(&g, firstReadProjects, errors.New("database is locked"))
	if firstReadsSettled(&g) {
		t.Fatal("a failed project read counted as the client having its catalog")
	}
	answerRead(&g, firstReadProjects, nil)
	if !firstReadsSettled(&g) {
		t.Fatal("did not settle once both catalogs answered")
	}
	// Settled stays settled, and a later wait returns without a timer.
	answerRead(&g, firstReadThreads, errors.New("later failure"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.await(ctx, time.Hour); err != nil {
		t.Fatalf("await on a settled gate = %v, want nil", err)
	}
}

func TestFirstReadsGateFallbackLetsAReadInFlightFinish(t *testing.T) {
	var g firstReadsGate
	end := g.read(firstReadThreads)
	g.expire()
	if firstReadsSettled(&g) {
		t.Fatal("the fallback settled over a catalog read still in flight")
	}
	failed := error(errors.New("read failed"))
	end(&failed)
	if !firstReadsSettled(&g) {
		t.Fatal("the fallback did not settle once the read in flight ended")
	}
}

func TestFirstReadsGateFallbackSettlesWithNoClient(t *testing.T) {
	var g firstReadsGate
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.await(ctx, 100*time.Millisecond); err != nil {
		t.Fatalf("await = %v, want the fallback to settle it", err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("settled after %v, before the fallback", elapsed)
	}
	if !firstReadsSettled(&g) {
		t.Fatal("await returned without settling the gate")
	}
}

// A settle by the reads releases a waiter at once rather than at its
// fallback, and ending the context releases one that is still waiting.
func TestFirstReadsGateWaitEndsWithTheReadsOrTheContext(t *testing.T) {
	var g firstReadsGate
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- g.await(ctx, time.Hour) }()
	answerRead(&g, firstReadProjects, nil)
	answerRead(&g, firstReadThreads, nil)
	if err := <-waited; err != nil {
		t.Fatalf("await = %v, want the reads to settle it", err)
	}

	var idle firstReadsGate
	stopped, stop := context.WithCancel(context.Background())
	go func() { waited <- idle.await(stopped, time.Hour) }()
	stop()
	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("await after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("await did not return after its context ended")
	}
	if firstReadsSettled(&idle) {
		t.Fatal("a cancelled wait settled the gate")
	}
}

func searchIndexBuilding(t *testing.T, app *App) bool {
	t.Helper()
	building, err := app.store.SearchIndexing()
	if err != nil {
		t.Fatalf("SearchIndexing: %v", err)
	}
	return building
}

func awaitSearchIndexBuilt(t *testing.T, app *App, within time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for searchIndexBuilding(t, app) {
		if time.Now().After(deadline) {
			t.Fatalf("the search index build did not finish within %v", within)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return time.Now()
}

// The boot build waits for the first client's catalog reads, and begins
// once both have answered.
func TestThreadSearchIndexWaitsForTheFirstCatalogReads(t *testing.T) {
	app := newTestAppWithStore(t)
	app.maintenance.firstReadsFallback = time.Hour
	if !searchIndexBuilding(t, app) {
		t.Fatal("a fresh database reported no outstanding index build")
	}
	t.Cleanup(func() {
		app.appCancel()
		app.waitThreadSearchIndex()
	})

	app.startThreadSearchIndex()
	if _, err := app.ListThreads(); err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	// Only the thread catalog has answered: the build must not run.
	for until := time.Now().Add(300 * time.Millisecond); time.Now().Before(until); {
		if !searchIndexBuilding(t, app) {
			t.Fatal("the index build ran before the project catalog answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := app.ListProjects(); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	awaitSearchIndexBuilt(t, app, 10*time.Second)
}

// With no client, the boot build begins at the fallback and not before.
func TestThreadSearchIndexBeginsAtTheFallbackWithNoClient(t *testing.T) {
	app := newTestAppWithStore(t)
	const fallback = 300 * time.Millisecond
	app.maintenance.firstReadsFallback = fallback
	t.Cleanup(func() {
		app.appCancel()
		app.waitThreadSearchIndex()
	})

	started := time.Now()
	app.startThreadSearchIndex()
	built := awaitSearchIndexBuilt(t, app, 10*time.Second)
	if elapsed := built.Sub(started); elapsed < fallback {
		t.Fatalf("the index build finished %v after start, before the %v fallback", elapsed, fallback)
	}
}

// Shutdown releases a build still waiting for its first reads.
func TestThreadSearchIndexWaitEndsAtShutdown(t *testing.T) {
	app := newTestAppWithStore(t)
	app.maintenance.firstReadsFallback = time.Hour
	app.startThreadSearchIndex()
	app.appCancel()
	joined := make(chan struct{})
	go func() {
		app.waitThreadSearchIndex()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("the join did not return after the app context ended")
	}
	if !searchIndexBuilding(t, app) {
		t.Fatal("a build cancelled while waiting still ran")
	}
}

// Only a client's bound catalog reads release the gate: the harness lists
// threads for itself without counting as one.
func TestHarnessThreadListIsNotAClientCatalogRead(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := (&harnessHost{app: app}).ListVisibleThreads(); err != nil {
		t.Fatalf("ListVisibleThreads: %v", err)
	}
	app.firstReads.mu.Lock()
	answered, inFlight := app.firstReads.answered, app.firstReads.inFlight
	app.firstReads.mu.Unlock()
	if answered != 0 || inFlight != 0 {
		t.Fatalf("a harness listing reached the gate: answered=%b inFlight=%d", answered, inFlight)
	}
	if _, err := app.ListThreads(); err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	app.firstReads.mu.Lock()
	answered = app.firstReads.answered
	app.firstReads.mu.Unlock()
	if answered != firstReadThreads {
		t.Fatalf("a bound ListThreads answer was not recorded: answered=%b", answered)
	}
}

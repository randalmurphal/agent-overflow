package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
)

// Two description edits of one PR opened on the same text: the second
// waits for the first's write and then re-reads, so the precondition
// refuses it instead of both passing it. The PR is one PR however its
// owner and repository are cased, as both forges resolve them.
func TestOverlappingDescriptionEditsRunOneAtATime(t *testing.T) {
	t.Parallel()
	shouted := testGitLabPR
	shouted.Namespace, shouted.Repo = "Group", "Widget"
	for _, tc := range []struct {
		name          string
		first, second gitops.PRReference
	}{
		{"one spelling", testGitLabPR, testGitLabPR},
		{"two spellings", testGitLabPR, shouted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			overlappingDescriptionEdits(t, tc.first, tc.second)
		})
	}
}

func overlappingDescriptionEdits(t *testing.T, firstPR, secondPR gitops.PRReference) {
	var mu sync.Mutex
	description := "old"
	inPut := make(chan struct{})
	release := make(chan struct{})
	puts, readsDuringPut := 0, 0
	putting := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/merge_requests/12") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			puts++
			first := puts == 1
			putting = true
			mu.Unlock()
			if first {
				close(inPut)
				<-release
			}
			mu.Lock()
			description, putting = body["description"], false
			mu.Unlock()
			_, _ = w.Write([]byte(`{"iid":12}`))
			return
		}
		mu.Lock()
		if putting {
			readsDuringPut++
		}
		current := description
		mu.Unlock()
		out, _ := json.Marshal(map[string]any{"iid": 12, "title": "T", "description": current, "state": "opened", "sha": "h"})
		_, _ = w.Write(out)
	})
	app := NewApp()
	isolatedForgeCore(t, app, handler)
	edit := func(pr gitops.PRReference, text string) error {
		expected := "old"
		_, err := app.EditPR(context.Background(), pr, gitops.PREdit{Description: &text, ExpectedDescription: &expected})
		return err
	}
	first := make(chan error, 1)
	go func() { first <- edit(firstPR, "from A") }()
	<-inPut
	second := make(chan error, 1)
	go func() { second <- edit(secondPR, "from B") }()
	// B waits on A's write; give it the time to (wrongly) read.
	time.Sleep(100 * time.Millisecond)
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first edit: %v", err)
	}
	err := <-second
	if !errors.Is(err, gitops.ErrDescriptionChanged) {
		t.Fatalf("second edit = %v, want ErrDescriptionChanged", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if readsDuringPut != 0 || puts != 1 || description != "from A" {
		t.Fatalf("reads during the first write %d, writes %d, description %q", readsDuringPut, puts, description)
	}
}

// The lock is held per PR, waits for its holder, gives up with the
// caller's context, and forgets a PR nobody holds.
func TestPRManageLocks(t *testing.T) {
	t.Parallel()
	var locks prManageLocks
	release, err := locks.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := locks.acquire(context.Background(), "b")
	if err != nil {
		t.Fatalf("another PR waited: %v", err)
	}
	other()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := locks.acquire(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock = %v, want the context's end", err)
	}
	got := make(chan error, 1)
	go func() {
		next, err := locks.acquire(context.Background(), "a")
		if err == nil {
			next()
		}
		got <- err
	}()
	release()
	if err := <-got; err != nil {
		t.Fatalf("the waiter: %v", err)
	}
	if n := locks.size(); n != 0 {
		t.Fatalf("entries after every release = %d", n)
	}
}

// A caller whose context ended takes no lock and starts no write, with
// the slot free before it asked or freed as its context ended; its entry
// is forgotten.
func TestPRManageLocksRefuseAnEndedContext(t *testing.T) {
	t.Parallel()
	var locks prManageLocks
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// It never enters the wait, so it can never take the slot.
	locks.woke = func() { t.Error("a call whose context had ended waited for the lock") }
	for range 20 {
		if _, err := locks.acquire(canceled, "a"); !errors.Is(err, context.Canceled) {
			t.Fatalf("an ended context on a free lock = %v, want its error", err)
		}
	}
	if n := locks.size(); n != 0 {
		t.Fatalf("entries after a refused call = %d", n)
	}
	locks.woke = nil
	// The context ends as the waiter takes the slot its holder released.
	release, err := locks.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	locks.woke = cancel
	got := make(chan error, 1)
	go func() {
		next, err := locks.acquire(ctx, "a")
		if err == nil {
			next()
		}
		got <- err
	}()
	for locks.users("a") < 2 {
		time.Sleep(time.Millisecond)
	}
	release()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("a waiter whose context ended as it took the slot = %v, want its error", err)
	}
	if n := locks.size(); n != 0 {
		t.Fatalf("entries after the waiter left = %d", n)
	}
	if next, err := locks.acquire(context.Background(), "a"); err != nil {
		t.Fatalf("the lock after the waiter left = %v", err)
	} else {
		next()
	}
	var app App
	wrote := false
	if _, err := app.runPRManage(canceled, testGitLabPR, gitops.ActionEditPR, func(context.Context) error { wrote = true; return nil }); !errors.Is(err, context.Canceled) || wrote {
		t.Fatalf("a canceled management call = %v, wrote %v", err, wrote)
	}
}

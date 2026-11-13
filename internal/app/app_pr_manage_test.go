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
// refuses it instead of both passing it.
func TestOverlappingDescriptionEditsRunOneAtATime(t *testing.T) {
	t.Parallel()
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
	edit := func(text string) error {
		expected := "old"
		_, err := app.EditPR(context.Background(), testGitLabPR, gitops.PREdit{Description: &text, ExpectedDescription: &expected})
		return err
	}
	first := make(chan error, 1)
	go func() { first <- edit("from A") }()
	<-inPut
	second := make(chan error, 1)
	go func() { second <- edit("from B") }()
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

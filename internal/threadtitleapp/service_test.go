package threadtitleapp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtitle"
)

type fakeStore struct {
	mu         sync.Mutex
	threads    map[string]store.Thread
	items      map[string][]store.Item
	dropped    bool
	contextErr error
}

func newFakeStore(threads ...store.Thread) *fakeStore {
	rows := make(map[string]store.Thread, len(threads))
	for _, thread := range threads {
		rows[thread.ID] = thread
	}
	return &fakeStore{threads: rows, items: make(map[string][]store.Item)}
}

func (s *fakeStore) GetThread(threadID string) (store.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread, ok := s.threads[threadID]
	if !ok {
		return store.Thread{}, errors.New("thread not found")
	}
	return thread, nil
}

func (s *fakeStore) ThreadTitleContextItems(threadID string, _ int) ([]store.Item, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.contextErr != nil {
		return nil, false, s.contextErr
	}
	return append([]store.Item(nil), s.items[threadID]...), s.dropped, nil
}

func (s *fakeStore) UpdateTitleIfCurrent(threadID, expectedTitle, title string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread, ok := s.threads[threadID]
	if !ok {
		return false, errors.New("thread not found")
	}
	if thread.Title != expectedTitle {
		return false, nil
	}
	thread.Title = title
	s.threads[threadID] = thread
	return true, nil
}

func (s *fakeStore) rename(threadID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread := s.threads[threadID]
	thread.Title = title
	s.threads[threadID] = thread
}

type recordedEvents struct {
	mu    sync.Mutex
	order []string
	done  []Completion
}

func (e *recordedEvents) applied(Applied) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.order = append(e.order, "applied")
}

func (e *recordedEvents) completed(completion Completion) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.order = append(e.order, "completed")
	e.done = append(e.done, completion)
}

func (e *recordedEvents) waitFor(t *testing.T, count int) []Completion {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		if len(e.done) >= count {
			out := append([]Completion(nil), e.done...)
			e.mu.Unlock()
			return out
		}
		e.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("completion count did not reach %d", count)
		}
		time.Sleep(time.Millisecond)
	}
}

func testThread(id, title string) store.Thread {
	return store.Thread{ID: id, Title: title}
}

func TestAutoAppliesSanitizedTitleBeforeCompletion(t *testing.T) {
	thread := testThread("thread-auto", threadtitle.Default)
	database := newFakeStore(thread)
	events := &recordedEvents{}
	service := New(Config{
		Store: database,
		Generate: func(got store.Thread, prompt string, imagePaths []string) (string, error) {
			if got.ID != thread.ID {
				t.Fatalf("thread = %q, want %q", got.ID, thread.ID)
			}
			if !strings.Contains(prompt, "User message:\nfix reconnect") {
				t.Fatalf("prompt missing raw message: %q", prompt)
			}
			if len(imagePaths) != 0 {
				t.Fatalf("image paths = %v, want none", imagePaths)
			}
			return ` "Reconnect spinner fix" `, nil
		},
		Applied:   events.applied,
		Completed: events.completed,
	})

	service.Auto(thread, "fix reconnect", nil, false)
	completion := events.waitFor(t, 1)[0]
	if completion.Error != "" {
		t.Fatalf("completion error = %q", completion.Error)
	}
	stored, _ := database.GetThread(thread.ID)
	if stored.Title != "Reconnect spinner fix" {
		t.Fatalf("title = %q", stored.Title)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if got := strings.Join(events.order, ","); got != "applied,completed" {
		t.Fatalf("event order = %q", got)
	}
}

func TestAutoSkipsCustomTitlesAndBlankMessages(t *testing.T) {
	tests := []struct {
		name    string
		thread  store.Thread
		message string
	}{
		{name: "custom title", thread: testThread("custom", "User title"), message: "an ask"},
		{name: "blank message", thread: testThread("blank", threadtitle.Default), message: " \n\t"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			service := New(Config{
				Store: newFakeStore(test.thread),
				Generate: func(store.Thread, string, []string) (string, error) {
					calls.Add(1)
					return "Unexpected", nil
				},
			})
			service.Auto(test.thread, test.message, nil, false)
			time.Sleep(20 * time.Millisecond)
			if got := calls.Load(); got != 0 {
				t.Fatalf("generator calls = %d, want 0", got)
			}
		})
	}
}

func TestAutoAndRegenerateShareOneInFlightRun(t *testing.T) {
	thread := testThread("thread-join", threadtitle.Default)
	database := newFakeStore(thread)
	events := &recordedEvents{}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	service := New(Config{
		Store: database,
		Generate: func(store.Thread, string, []string) (string, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return "Only title", nil
		},
		Completed: events.completed,
	})

	service.Auto(thread, "first send", nil, false)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("auto generation did not start")
	}
	if err := service.Regenerate(thread.ID); err != nil {
		t.Fatalf("Regenerate() error = %v", err)
	}
	service.Auto(thread, "second send", nil, false)
	close(release)
	events.waitFor(t, 1)
	if got := calls.Load(); got != 1 {
		t.Fatalf("generator calls = %d, want 1", got)
	}
	time.Sleep(20 * time.Millisecond)
	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.done) != 1 {
		t.Fatalf("completion events = %d, want 1", len(events.done))
	}
}

func TestFailedRunReleasesClaimForHeal(t *testing.T) {
	thread := testThread("thread-retry", threadtitle.Default)
	database := newFakeStore(thread)
	events := &recordedEvents{}
	var calls atomic.Int64
	service := New(Config{
		Store: database,
		Generate: func(store.Thread, string, []string) (string, error) {
			if calls.Add(1) == 1 {
				return "", errors.New("claude CLI failed: secret prompt")
			}
			return "Second try", nil
		},
		Completed: events.completed,
	})

	service.Auto(thread, "first send", nil, false)
	first := events.waitFor(t, 1)[0]
	if first.Error != "provider CLI failed" {
		t.Fatalf("first error = %q", first.Error)
	}
	service.Auto(thread, "second send", nil, false)
	done := events.waitFor(t, 2)
	if done[1].Error != "" {
		t.Fatalf("second error = %q", done[1].Error)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("generator calls = %d, want 2", got)
	}
}

func TestAutoHealUsesConversationAndFallsBackOnContextError(t *testing.T) {
	thread := testThread("thread-heal", threadtitle.Default)
	database := newFakeStore(thread)
	database.items[thread.ID] = []store.Item{{
		ID: "user:0", Kind: "user_text", Role: "user", Summary: "the original ask",
	}}
	requests := make(chan string, 2)
	events := &recordedEvents{}
	service := New(Config{
		Store: database,
		Generate: func(_ store.Thread, prompt string, _ []string) (string, error) {
			requests <- prompt
			return threadtitle.Default, nil
		},
		Completed: events.completed,
	})

	service.Auto(thread, "later tangent", nil, true)
	events.waitFor(t, 1)
	if prompt := <-requests; !strings.Contains(prompt, "Thread contents:\nUSER:\nthe original ask") || strings.Contains(prompt, "later tangent") {
		t.Fatalf("heal prompt = %q", prompt)
	}

	database.contextErr = errors.New("read failed")
	service.Auto(thread, "fallback message", nil, true)
	events.waitFor(t, 2)
	if prompt := <-requests; !strings.Contains(prompt, "User message:\nfallback message") {
		t.Fatalf("fallback prompt = %q", prompt)
	}
}

func TestRegenerateRereadsTitleAndPreservesRenameThatWinsCAS(t *testing.T) {
	thread := testThread("thread-cas", "Stale title")
	database := newFakeStore(thread)
	database.items[thread.ID] = []store.Item{{Kind: "user_text", Role: "user", Summary: "the ask"}}
	events := &recordedEvents{}
	started := make(chan struct{})
	release := make(chan struct{})
	service := New(Config{
		Store: database,
		Generate: func(_ store.Thread, prompt string, _ []string) (string, error) {
			if !strings.Contains(prompt, `The previous title was "Stale title".`) {
				t.Fatalf("prompt = %q", prompt)
			}
			close(started)
			<-release
			return "Generated title", nil
		},
		Completed: events.completed,
	})

	if err := service.Regenerate(thread.ID); err != nil {
		t.Fatalf("Regenerate() error = %v", err)
	}
	<-started
	database.rename(thread.ID, "User title")
	close(release)
	if completion := events.waitFor(t, 1)[0]; completion.Error != "" {
		t.Fatalf("completion error = %q", completion.Error)
	}
	stored, _ := database.GetThread(thread.ID)
	if stored.Title != "User title" {
		t.Fatalf("title = %q, want user rename", stored.Title)
	}
}

func TestRegenerateSecondRunReadsTheFirstResult(t *testing.T) {
	thread := testThread("thread-second", "Stale title")
	database := newFakeStore(thread)
	database.items[thread.ID] = []store.Item{{Kind: "user_text", Role: "user", Summary: "the ask"}}
	events := &recordedEvents{}
	requests := make(chan string, 2)
	var calls atomic.Int64
	service := New(Config{
		Store: database,
		Generate: func(_ store.Thread, prompt string, _ []string) (string, error) {
			requests <- prompt
			if calls.Add(1) == 1 {
				return "First result", nil
			}
			return "Second result", nil
		},
		Completed: events.completed,
	})

	if err := service.Regenerate(thread.ID); err != nil {
		t.Fatal(err)
	}
	events.waitFor(t, 1)
	if err := service.Regenerate(thread.ID); err != nil {
		t.Fatal(err)
	}
	events.waitFor(t, 2)
	first, second := <-requests, <-requests
	if !strings.Contains(first, `The previous title was "Stale title".`) {
		t.Fatalf("first prompt = %q", first)
	}
	if !strings.Contains(second, `The previous title was "First result".`) {
		t.Fatalf("second prompt = %q", second)
	}
}

func TestRegenerateNoConversationAndNoBetterTitleAreCleanNoOps(t *testing.T) {
	t.Run("no conversation", func(t *testing.T) {
		thread := testThread("empty", "Existing title")
		events := &recordedEvents{}
		var calls atomic.Int64
		service := New(Config{
			Store: newFakeStore(thread),
			Generate: func(store.Thread, string, []string) (string, error) {
				calls.Add(1)
				return "Unexpected", nil
			},
			Completed: events.completed,
		})
		if err := service.Regenerate(thread.ID); err != nil {
			t.Fatal(err)
		}
		if completion := events.waitFor(t, 1)[0]; completion.Error != "" {
			t.Fatalf("completion error = %q", completion.Error)
		}
		if got := calls.Load(); got != 0 {
			t.Fatalf("generator calls = %d, want 0", got)
		}
	})

	for name, answer := range map[string]string{
		"same":    "Existing title",
		"empty":   "  ",
		"default": threadtitle.Default,
	} {
		t.Run(name, func(t *testing.T) {
			thread := testThread("noop-"+name, "Existing title")
			database := newFakeStore(thread)
			database.items[thread.ID] = []store.Item{{Kind: "user_text", Role: "user", Summary: "the ask"}}
			events := &recordedEvents{}
			service := New(Config{
				Store:     database,
				Generate:  func(store.Thread, string, []string) (string, error) { return answer, nil },
				Completed: events.completed,
			})
			if err := service.Regenerate(thread.ID); err != nil {
				t.Fatal(err)
			}
			events.waitFor(t, 1)
			stored, _ := database.GetThread(thread.ID)
			if stored.Title != "Existing title" {
				t.Fatalf("title = %q", stored.Title)
			}
		})
	}
}

func TestRegenerateKeepsTextWhenUserMetadataIsCorrupt(t *testing.T) {
	thread := testThread("corrupt-meta", "Existing title")
	database := newFakeStore(thread)
	database.items[thread.ID] = []store.Item{{
		ID: "user:0", Kind: "user_text", Role: "user", Summary: "the ask survives",
		Meta: `{"attachments":"not-an-array"}`,
	}}
	events := &recordedEvents{}
	service := New(Config{
		Store: database,
		Generate: func(_ store.Thread, prompt string, _ []string) (string, error) {
			if !strings.HasSuffix(prompt, "Thread contents:\nUSER:\nthe ask survives") {
				t.Fatalf("prompt = %q", prompt)
			}
			return "Survived metadata", nil
		},
		Completed: events.completed,
	})
	if err := service.Regenerate(thread.ID); err != nil {
		t.Fatal(err)
	}
	events.waitFor(t, 1)
	stored, _ := database.GetThread(thread.ID)
	if stored.Title != "Survived metadata" {
		t.Fatalf("title = %q", stored.Title)
	}
}

type fakeAttachments struct {
	rows map[string]struct {
		record store.Attachment
		path   string
	}
}

func (a *fakeAttachments) Get(id string) (store.Attachment, string, bool, error) {
	row, ok := a.rows[id]
	return row.record, row.path, ok, nil
}

func TestAutoPassesOnlyOwnedExistingImagePaths(t *testing.T) {
	thread := testThread("thread-images", threadtitle.Default)
	ownedPath := filepath.Join(t.TempDir(), "owned.png")
	if err := os.WriteFile(ownedPath, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(t.TempDir(), "missing.png")
	attachments := []store.Attachment{
		{ID: "owned", ThreadID: thread.ID, Filename: "owned.png", MimeType: "image/png"},
		{ID: "foreign", ThreadID: thread.ID, Filename: "foreign.png", MimeType: "image/png"},
		{ID: "missing", ThreadID: thread.ID, Filename: "missing.png", MimeType: "image/png"},
		// A `file` kind that EXISTS on disk and belongs to the thread, so
		// the only thing that can exclude it is the kind check.
		{ID: "text", ThreadID: thread.ID, Filename: "notes.txt", MimeType: "text/plain", Kind: store.AttachmentKindFile},
	}
	resolved := &fakeAttachments{rows: map[string]struct {
		record store.Attachment
		path   string
	}{
		"owned":   {record: attachments[0], path: ownedPath},
		"foreign": {record: store.Attachment{ID: "foreign", ThreadID: "other"}, path: ownedPath},
		"missing": {record: attachments[2], path: missingPath},
		"text":    {record: attachments[3], path: ownedPath},
	}}
	events := &recordedEvents{}
	service := New(Config{
		Store:       newFakeStore(thread),
		Attachments: resolved,
		Generate: func(_ store.Thread, prompt string, paths []string) (string, error) {
			if len(paths) != 1 || paths[0] != ownedPath {
				t.Fatalf("image paths = %v; want only the owned, existing IMAGE row", paths)
			}
			if !strings.Contains(prompt, "Attachment metadata:") {
				t.Fatalf("prompt missing attachment metadata: %q", prompt)
			}
			return "Image title", nil
		},
		Completed: events.completed,
	})
	service.Auto(thread, "fix screenshot", attachments, false)
	events.waitFor(t, 1)
}

func TestRegenerateUnknownThreadIsTheOnlySynchronousFailure(t *testing.T) {
	service := New(Config{Store: newFakeStore()})
	if err := service.Regenerate("missing"); err == nil || !strings.Contains(err.Error(), "regenerate thread title") {
		t.Fatalf("Regenerate() error = %v", err)
	}
}

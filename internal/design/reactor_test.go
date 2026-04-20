package design

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

type designEmission struct {
	name string
	data any
}

func TestReactorRenderStoresArtifactAndEmitsEvent(t *testing.T) {
	reactor, collector := newTestReactor(t)

	artifact, err := reactor.Render("thread-render", RenderInput{
		HTML:        "<html><body>render</body></html>",
		Title:       "Homepage",
		Description: "Primary direction",
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if artifact.Kind != "render" {
		t.Fatalf("artifact kind = %q, want render", artifact.Kind)
	}
	emissions := collector.getEmissions()
	if len(emissions) != 1 {
		t.Fatalf("emissions len = %d, want 1", len(emissions))
	}
	if emissions[0].name != artifactRenderedEvent {
		t.Fatalf("event name = %q, want %q", emissions[0].name, artifactRenderedEvent)
	}

	storedArtifact, ok := emissions[0].data.(DesignArtifact)
	if !ok {
		t.Fatalf("event payload type = %T, want DesignArtifact", emissions[0].data)
	}
	if storedArtifact.ID != artifact.ID {
		t.Fatalf("emitted artifact ID = %q, want %q", storedArtifact.ID, artifact.ID)
	}
}

func TestReactorPresentOptionsCreatesInteractiveRequest(t *testing.T) {
	reactor, collector := newTestReactor(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resultCh := make(chan ChoiceResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := reactor.PresentOptions(ctx, "thread-options", PresentOptionsInput{
			Prompt: "Choose a direction",
			Options: []PresentOptionInput{
				{
					ID:          "a",
					Title:       "Minimal",
					Description: "Cleaner layout",
					HTML:        "<html><body>A</body></html>",
				},
				{
					ID:          "b",
					Title:       "Editorial",
					Description: "Heavier typography",
					HTML:        "<html><body>B</body></html>",
				},
			},
		})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- result
	}()

	request := waitForOptionsRequest(t, collector)
	if request.ThreadID != "thread-options" {
		t.Fatalf("ThreadID = %q, want thread-options", request.ThreadID)
	}
	if len(request.Options) != 2 {
		t.Fatalf("options len = %d, want 2", len(request.Options))
	}
	if request.Options[0].ArtifactID == "" || request.Options[1].ArtifactID == "" {
		t.Fatal("expected persisted artifact IDs for options")
	}

	artifacts, err := reactor.artifacts.List("thread-options", "option")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("stored option artifacts = %d, want 2", len(artifacts))
	}

	if err := reactor.ChooseOption("thread-options", request.RequestID, "b"); err != nil {
		t.Fatalf("ChooseOption() error = %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("PresentOptions() error = %v", err)
	case result := <-resultCh:
		if result.Chosen != "b" {
			t.Fatalf("Chosen = %q, want b", result.Chosen)
		}
		if result.Title != "Editorial" {
			t.Fatalf("Title = %q, want Editorial", result.Title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for option resolution")
	}
}

func TestReactorTeardownThreadCancelsPendingChoices(t *testing.T) {
	reactor, _ := newTestReactor(t)

	errCh := make(chan error, 1)
	go func() {
		_, err := reactor.PresentOptions(context.Background(), "thread-cancel", PresentOptionsInput{
			Prompt: "Choose",
			Options: []PresentOptionInput{
				{ID: "a", Title: "A", Description: "Alpha", HTML: "<html>A</html>"},
				{ID: "b", Title: "B", Description: "Beta", HTML: "<html>B</html>"},
			},
		})
		errCh <- err
	}()

	waitForPendingRequest(t, reactor, "thread-cancel")
	reactor.TeardownThread("thread-cancel")

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected teardown error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for teardown")
	}
}

func TestReactorPendingRequestReturnsStoredRequest(t *testing.T) {
	reactor, _ := newTestReactor(t)

	go func() {
		_, _ = reactor.PresentOptions(context.Background(), "thread-options", PresentOptionsInput{
			Prompt: "Choose",
			Options: []PresentOptionInput{
				{ID: "a", Title: "A", Description: "Alpha", HTML: "<html>A</html>"},
				{ID: "b", Title: "B", Description: "Beta", HTML: "<html>B</html>"},
			},
		})
	}()

	request := waitForPendingRequest(t, reactor, "thread-options")
	if request.Prompt != "Choose" {
		t.Fatalf("Prompt = %q, want Choose", request.Prompt)
	}
	if _, ok := reactor.PendingRequest("missing-thread"); ok {
		t.Fatal("expected no pending request for missing thread")
	}
	if err := reactor.ChooseOption("thread-options", request.RequestID, "a"); err != nil {
		t.Fatalf("ChooseOption() error = %v", err)
	}
}

func TestReactorPresentOptionsContextCancellation(t *testing.T) {
	reactor, _ := newTestReactor(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := reactor.PresentOptions(ctx, "thread-options", PresentOptionsInput{
		Prompt: "Choose",
		Options: []PresentOptionInput{
			{ID: "a", Title: "A", Description: "Alpha", HTML: "<html>A</html>"},
			{ID: "b", Title: "B", Description: "Beta", HTML: "<html>B</html>"},
		},
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if _, ok := reactor.PendingRequest("thread-options"); ok {
		t.Fatal("expected pending request to be cleared after cancellation")
	}
}

func TestReactorValidationErrors(t *testing.T) {
	reactor, _ := newTestReactor(t)

	if _, err := reactor.Render("", RenderInput{HTML: "<html></html>", Title: "Title"}); err == nil {
		t.Fatal("expected missing thread ID error")
	}
	if _, err := reactor.PresentOptions(context.Background(), "thread-options", PresentOptionsInput{
		Prompt: "Choose",
		Options: []PresentOptionInput{
			{ID: "a", Title: "A", Description: "Alpha", HTML: "<html>A</html>"},
		},
	}); err == nil {
		t.Fatal("expected at least 2 options error")
	}
	if _, err := reactor.PresentOptions(context.Background(), "thread-options", PresentOptionsInput{
		Prompt: "Choose",
		Options: []PresentOptionInput{
			{ID: "dup", Title: "A", Description: "Alpha", HTML: "<html>A</html>"},
			{ID: "dup", Title: "B", Description: "Beta", HTML: "<html>B</html>"},
		},
	}); err == nil {
		t.Fatal("expected duplicate option ID error")
	}
	assertOptionArtifactCount(t, reactor, "thread-options", 0)
	if _, err := reactor.PresentOptions(context.Background(), "thread-options", PresentOptionsInput{
		Prompt: "Choose",
		Options: []PresentOptionInput{
			{ID: "a", Title: "A", Description: "Alpha", HTML: "<html>A</html>"},
			{ID: "b", Title: "", Description: "Beta", HTML: "<html>B</html>"},
		},
	}); err == nil {
		t.Fatal("expected missing title error")
	}
	assertOptionArtifactCount(t, reactor, "thread-options", 0)
	if err := reactor.ChooseOption("thread-options", "missing", "a"); err == nil {
		t.Fatal("expected missing request error")
	}
}

type emissionCollector struct {
	mu        sync.Mutex
	emissions []designEmission
}

func (ec *emissionCollector) append(name string, data any) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	ec.emissions = append(ec.emissions, designEmission{name: name, data: data})
}

func (ec *emissionCollector) getEmissions() []designEmission {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	out := make([]designEmission, len(ec.emissions))
	copy(out, ec.emissions)
	return out
}

func newTestReactor(t *testing.T) (*Reactor, *emissionCollector) {
	t.Helper()

	st := newDesignTestStore(t)
	collector := &emissionCollector{}
	reactor := NewReactor(
		NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), st),
		collector.append,
	)
	return reactor, collector
}

func newDesignTestStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.New(filepath.Join(t.TempDir(), "design.db"))
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	project := testutil.EnsureProject(t, st, t.TempDir())
	thread := store.Thread{
		ID:            "thread-render",
		ProjectID:     project.ID,
		Title:         "Render",
		Provider:      "codex",
		WorkspacePath: t.TempDir(),
		Model:         "gpt-5.4",
		Mode:          "design",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(thread-render) error = %v", err)
	}

	for _, threadID := range []string{"thread-options", "thread-cancel"} {
		thread.ID = threadID
		if err := st.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread(%s) error = %v", threadID, err)
		}
	}

	return st
}

func waitForOptionsRequest(t *testing.T, collector *emissionCollector) DesignOptionsRequest {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, emission := range collector.getEmissions() {
			if emission.name != optionsPresentedEvent {
				continue
			}
			request, ok := emission.data.(DesignOptionsRequest)
			if !ok {
				t.Fatalf("event payload type = %T, want DesignOptionsRequest", emission.data)
			}
			return request
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for design options event")
	return DesignOptionsRequest{}
}

func waitForPendingRequest(t *testing.T, reactor *Reactor, threadID string) DesignOptionsRequest {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if request, ok := reactor.PendingRequest(threadID); ok {
			return request
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for pending design request")
	return DesignOptionsRequest{}
}

func assertOptionArtifactCount(t *testing.T, reactor *Reactor, threadID string, want int) {
	t.Helper()

	artifacts, err := reactor.artifacts.List(threadID, "option")
	if err != nil {
		t.Fatalf("List(%s, option) error = %v", threadID, err)
	}
	if len(artifacts) != want {
		t.Fatalf("option artifact count for %s = %d, want %d", threadID, len(artifacts), want)
	}
}

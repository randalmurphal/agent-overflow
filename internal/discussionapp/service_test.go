package discussionapp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

type failingRuntime struct {
	starts  []string
	stops   []string
	cleared []string
}

func (r *failingRuntime) StartParticipant(_ context.Context, threadID, _ string) error {
	r.starts = append(r.starts, threadID)
	if len(r.starts) == 2 {
		return errors.New("participant start failed")
	}
	return nil
}

func (r *failingRuntime) StopParticipant(threadID string) error {
	r.stops = append(r.stops, threadID)
	return nil
}

func (r *failingRuntime) ClearParticipantPrompt(threadID string) {
	r.cleared = append(r.cleared, threadID)
}

func (*failingRuntime) SendParticipantMessage(string, string) error { return nil }

func TestStartFailureRollsBackEveryOwnedResource(t *testing.T) {
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UnixMilli()
	project := store.Project{ID: "project", Path: t.TempDir(), Name: "Project", CreatedAt: now, UpdatedAt: now}
	if _, err := database.CreateProject(project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	parent := store.Thread{
		ID: "parent", ProjectID: project.ID, Title: "Parent", Provider: "codex",
		Model: "gpt-5.4", WorkspacePath: project.Path, Mode: "chat", CreatedAt: now, UpdatedAt: now,
	}
	if err := database.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	definition := store.DiscussionDefinition{
		Name: "Review", Scope: "global", Settings: store.DiscussionSettings{MaxTurns: 4},
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design it"},
			{Role: "reviewer", System: "Review it"},
		},
	}
	runtime := &failingRuntime{}
	service := New(Config{Store: func() *store.Store { return database }, Runtime: runtime})
	if err := service.Create(definition); err != nil {
		t.Fatalf("Create definition: %v", err)
	}

	err = service.Start(parent.ID, definition.Name)
	if err == nil || !strings.Contains(err.Error(), "participant start failed") {
		t.Fatalf("Start() error = %v, want participant failure", err)
	}
	children, err := database.ListChildThreads(parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("children after rollback = %+v, want none", children)
	}
	storedParent, err := database.GetThread(parent.ID)
	if err != nil {
		t.Fatalf("GetThread(parent): %v", err)
	}
	if storedParent.Mode != "chat" || storedParent.DiscussionID != "" {
		t.Fatalf("parent after rollback = %+v, want untouched chat parent", storedParent)
	}
	if service.RuntimeCount() != 0 {
		t.Fatalf("RuntimeCount() = %d, want 0", service.RuntimeCount())
	}
	if len(runtime.stops) != 1 || runtime.stops[0] != runtime.starts[0] {
		t.Fatalf("stopped participants = %v, starts = %v", runtime.stops, runtime.starts)
	}
	for _, started := range runtime.starts {
		if !slices.Contains(runtime.cleared, started) {
			t.Fatalf("cleared prompts = %v, missing %s", runtime.cleared, started)
		}
	}
}

// The runtime ward is the state that motivated this extraction: every read,
// install, and removal must share one owner and one lock. This test is useful
// under -race; it also verifies that removal never leaves a second map behind.
func TestRuntimeWardConcurrentAccess(t *testing.T) {
	service := New(Config{})
	const channelID = "channel-race"
	runtime := discussion.NewDeliberation(channelID, 8, []string{"architect", "reviewer"})
	service.InstallRuntime(channelID, runtime)

	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(2)
		go func() {
			defer wait.Done()
			service.InstallRuntime(channelID, runtime)
			if current, ok := service.Runtime(channelID); ok {
				_ = current.State()
			}
		}()
		go func() {
			defer wait.Done()
			service.Remove(channelID)
		}()
	}
	wait.Wait()

	service.InstallRuntime(channelID, runtime)
	got, ok := service.Runtime(channelID)
	if !ok || got != runtime {
		t.Fatalf("Runtime() = (%p, %v), want installed runtime %p", got, ok, runtime)
	}
	if got := service.RuntimeCount(); got != 1 {
		t.Fatalf("RuntimeCount() = %d, want 1", got)
	}
}

func TestRebuildCurrentSpeakerContinuesRoundRobin(t *testing.T) {
	participants := []string{"architect", "reviewer", "operator"}
	tests := []struct {
		name       string
		lastPoster string
		want       string
	}{
		{name: "fresh", want: "architect"},
		{name: "middle", lastPoster: "reviewer", want: "operator"},
		{name: "wrap", lastPoster: "operator", want: "architect"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := rebuildCurrentSpeaker(participants, test.lastPoster); got != test.want {
				t.Fatalf("rebuildCurrentSpeaker() = %q, want %q", got, test.want)
			}
		})
	}
}

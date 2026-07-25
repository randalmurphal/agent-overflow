package store

import (
	"testing"

	"agent-overflow/internal/threadmode"
)

func TestHiddenWorkflowModesStayOutOfNormalListsSearchAndCounts(t *testing.T) {
	s := newTestStore(t)
	modes := []string{
		threadmode.ModeChat,
		threadmode.ModeWorkflow,
		threadmode.ModeWorkflowStudio,
		threadmode.ModeWorkflowTriage,
	}
	for index, mode := range modes {
		thread := makeThread("mode-"+mode, "codex")
		thread.Mode = mode
		thread.Title = "needle " + mode
		thread.UpdatedAt += int64(index)
		if err := s.CreateThread(thread); err != nil {
			t.Fatalf("create %s: %v", mode, err)
		}
		if err := s.InsertItem(Item{
			ID: "item-" + mode, ThreadID: thread.ID, Kind: "assistant_text",
			Role: "assistant", Status: "completed", Summary: "needle body " + mode,
			CreatedAt: int64(index + 1), UpdatedAt: int64(index + 1),
		}); err != nil {
			t.Fatalf("insert %s item: %v", mode, err)
		}
	}

	listed, err := s.ListThreadsWithItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Mode != threadmode.ModeChat {
		t.Fatalf("ListThreadsWithItems = %+v, want only chat", listed)
	}
	projectThreads, err := s.ListThreadsByProject(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectThreads) != 1 || projectThreads[0].Mode != threadmode.ModeChat {
		t.Fatalf("ListThreadsByProject = %+v, want only chat", projectThreads)
	}
	hits, err := s.SearchThreadMessages("needle", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.ThreadID != "mode-chat" {
			t.Fatalf("global search surfaced hidden thread: %+v", hit)
		}
	}
	projects, err := s.ListProjectsWithThreadCounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].ThreadCount != 1 {
		t.Fatalf("project counts = %+v, want one normal thread", projects)
	}
}

func TestFindWorkflowTriageAgentExcludesItemLinkedThreads(t *testing.T) {
	s := newTestStore(t)
	itemThread := makeThread("item-triage", "codex")
	itemThread.Mode = threadmode.ModeWorkflowTriage
	agentThread := makeThread("agent-triage", "codex")
	agentThread.Mode = threadmode.ModeWorkflowTriage
	agentThread.CreatedAt++
	for _, thread := range []Thread{itemThread, agentThread} {
		if err := s.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	item := testWorkItem("workflow-item", defaultTestProjectID, "needs-human", 1)
	item.Reason = "stuck"
	item.TriageThreadID = itemThread.ID
	if err := s.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.FindWorkflowTriageAgentThread(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.ID != agentThread.ID {
		t.Fatalf("triage agent = %+v found=%v, want %s", got, found, agentThread.ID)
	}
}

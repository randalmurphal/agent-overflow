package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// testAssistant records an assistant on an existing thread as v155 migrates
// a promoted one: its id is the thread's, its home is
// `<config>/agents/<id>`, made here, the thread moves into it, and it
// serves the thread's former project. A fixture with no config directory
// gets one.
func testAssistant(t *testing.T, a *App, threadID string) store.StandingAgent {
	t.Helper()
	if a.configDir == "" {
		a.configDir = t.TempDir()
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	agent := store.StandingAgent{ID: threadID, ThreadID: threadID, Home: filepath.Join(a.configDir, "agents", threadID), CreatedAt: time.Now().UnixMilli()}
	if err := os.MkdirAll(agent.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	var projects []string
	if thread.ProjectID != "" {
		projects = append(projects, thread.ProjectID)
	}
	if err := a.store.PlaceThreadInHome(threadID, agent.Home); err != nil {
		t.Fatalf("place %s in its home: %v", threadID, err)
	}
	if err := a.store.CreateStandingAgent(agent, projects...); err != nil {
		t.Fatalf("create assistant on %s: %v", threadID, err)
	}
	return agent
}

// placeInAssistantHome moves threadID into the home of the assistant whose
// current thread agentThreadID is, where a handoff's successor lives.
func placeInAssistantHome(t *testing.T, a *App, threadID, agentThreadID string) {
	t.Helper()
	agent, found, err := a.store.GetStandingAgent(agentThreadID)
	if err != nil || !found {
		t.Fatalf("the assistant of %s = %v %v", agentThreadID, found, err)
	}
	if err := a.store.PlaceThreadInHome(threadID, agent.Home); err != nil {
		t.Fatalf("place %s in %s: %v", threadID, agent.Home, err)
	}
}

// storeAgent is an assistant record on threadID with the given home, its id
// the thread's.
func storeAgent(threadID, home string) store.StandingAgent {
	return store.StandingAgent{ID: threadID, ThreadID: threadID, Home: home, CreatedAt: 1}
}

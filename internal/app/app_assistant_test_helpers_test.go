package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// testAssistant records an assistant on an existing thread as v155 migrates
// a promoted one: its id is the thread's and its home is
// `<config>/agents/<id>`, made here. A fixture with no config directory gets
// one.
func testAssistant(t *testing.T, a *App, threadID string) store.StandingAgent {
	t.Helper()
	if a.configDir == "" {
		a.configDir = t.TempDir()
	}
	agent := store.StandingAgent{ID: threadID, ThreadID: threadID, Home: filepath.Join(a.configDir, "agents", threadID), CreatedAt: time.Now().UnixMilli()}
	if err := os.MkdirAll(agent.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateStandingAgent(agent); err != nil {
		t.Fatalf("create assistant on %s: %v", threadID, err)
	}
	return agent
}

// storeAgent is an assistant record on threadID with the given home, its id
// the thread's.
func storeAgent(threadID, home string) store.StandingAgent {
	return store.StandingAgent{ID: threadID, ThreadID: threadID, Home: home, CreatedAt: 1}
}

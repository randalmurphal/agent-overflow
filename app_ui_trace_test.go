package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/uitrace"
)

func TestAppendUIRenderTraceBatchRoundtrip(t *testing.T) {
	app := &App{configDir: t.TempDir()}

	path, err := app.AppendUIRenderTraceBatch([]string{`{"label":"chat.state"}`})
	if err != nil {
		t.Fatalf("AppendUIRenderTraceBatch returned error: %v", err)
	}

	expectedPath := filepath.Join(app.configDir, uitrace.DirName, uitrace.FileName)
	if path != expectedPath {
		t.Fatalf("path = %q, want %q", path, expectedPath)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	if strings.TrimSpace(string(data)) != `{"label":"chat.state"}` {
		t.Fatalf("content = %q", data)
	}
}

func TestGetUIRenderTracePathRequiresConfigDir(t *testing.T) {
	app := &App{}

	_, err := app.GetUIRenderTracePath()
	if err == nil {
		t.Fatal("GetUIRenderTracePath returned nil error")
	}
}

func TestUITraceLazyInitMemoizes(t *testing.T) {
	app := &App{configDir: t.TempDir()}

	t1, err := app.uiTrace()
	if err != nil {
		t.Fatalf("uiTrace returned error: %v", err)
	}
	t2, err := app.uiTrace()
	if err != nil {
		t.Fatalf("uiTrace returned error: %v", err)
	}
	if t1 != t2 {
		t.Fatal("uiTrace returned distinct Tracers across calls")
	}
}

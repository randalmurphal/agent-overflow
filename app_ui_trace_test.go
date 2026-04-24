package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendUIRenderTraceBatchWritesJSONLines(t *testing.T) {
	app := &App{configDir: t.TempDir()}

	path, err := app.AppendUIRenderTraceBatch([]string{
		`{"label":"chat.state","data":{"threadId":"thread-1"}}`,
		`{"label":"chat.dom"}`,
	})
	if err != nil {
		t.Fatalf("AppendUIRenderTraceBatch returned error: %v", err)
	}

	expectedPath := filepath.Join(app.configDir, uiTraceDirName, uiTraceFileName)
	if path != expectedPath {
		t.Fatalf("path = %q, want %q", path, expectedPath)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(got) != 2 {
		t.Fatalf("line count = %d, want 2; data=%q", len(got), data)
	}
	if got[0] != `{"label":"chat.state","data":{"threadId":"thread-1"}}` {
		t.Fatalf("first line = %q", got[0])
	}
}

func TestAppendUIRenderTraceBatchRejectsInvalidJSON(t *testing.T) {
	app := &App{configDir: t.TempDir()}

	_, err := app.AppendUIRenderTraceBatch([]string{`{"label":`})
	if err == nil {
		t.Fatal("AppendUIRenderTraceBatch returned nil error")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error = %q, want invalid JSON message", err)
	}
}

func TestAppendUIRenderTraceBatchRejectsOversizedLine(t *testing.T) {
	app := &App{configDir: t.TempDir()}
	line := `{"value":"` + strings.Repeat("x", uiTraceMaxLineBytes) + `"}`

	_, err := app.AppendUIRenderTraceBatch([]string{line})
	if err == nil {
		t.Fatal("AppendUIRenderTraceBatch returned nil error")
	}
	if !strings.Contains(err.Error(), "max") {
		t.Fatalf("error = %q, want max size message", err)
	}
}

func TestAppendUIRenderTraceBatchRejectsOversizedBatch(t *testing.T) {
	app := &App{configDir: t.TempDir()}
	line := `{"value":"` + strings.Repeat("x", 3000) + `"}`
	lines := make([]string, 0, uiTraceMaxBatchLines)
	for i := 0; i < uiTraceMaxBatchLines; i++ {
		lines = append(lines, line)
	}

	_, err := app.AppendUIRenderTraceBatch(lines)
	if err == nil {
		t.Fatal("AppendUIRenderTraceBatch returned nil error")
	}
	if !strings.Contains(err.Error(), "batch") {
		t.Fatalf("error = %q, want batch size message", err)
	}
}

func TestGetUIRenderTracePathRequiresConfigDir(t *testing.T) {
	app := &App{}

	_, err := app.GetUIRenderTracePath()
	if err == nil {
		t.Fatal("GetUIRenderTracePath returned nil error")
	}
}

package app

// App-level coverage for the root observability façade.

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

func TestReportFrontendErrorBatchRoundtrip(t *testing.T) {
	app := &App{configDir: t.TempDir()}

	line := `{"kind":"error","message":"render boom","seen":1}`
	path, err := app.ReportFrontendErrorBatch([]string{line})
	if err != nil {
		t.Fatalf("ReportFrontendErrorBatch returned error: %v", err)
	}

	expectedPath := filepath.Join(app.configDir, uitrace.DirName, uitrace.ErrorFileName)
	if path != expectedPath {
		t.Fatalf("path = %q, want %q", path, expectedPath)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frontend error log: %v", err)
	}
	if strings.TrimSpace(string(data)) != line {
		t.Fatalf("content = %q", data)
	}

	// The error log and the render trace are separate files: appending an
	// error must not create or touch the render-trace file.
	if _, err := os.Stat(filepath.Join(app.configDir, uitrace.DirName, uitrace.FileName)); !os.IsNotExist(err) {
		t.Fatalf("render trace file unexpectedly present (err=%v)", err)
	}
}

func TestReportFrontendErrorBatchRequiresConfigDir(t *testing.T) {
	app := &App{}

	if _, err := app.ReportFrontendErrorBatch([]string{`{}`}); err == nil {
		t.Fatal("ReportFrontendErrorBatch returned nil error without configDir")
	}
}

func TestTraceChannelsAreIndependentOnOneApp(t *testing.T) {
	app := &App{configDir: t.TempDir()}

	tracePath, err := app.AppendUIRenderTraceBatch([]string{`{"label":"chat.state"}`})
	if err != nil {
		t.Fatalf("AppendUIRenderTraceBatch returned error: %v", err)
	}
	errPath, err := app.ReportFrontendErrorBatch([]string{`{"kind":"error"}`})
	if err != nil {
		t.Fatalf("ReportFrontendErrorBatch returned error: %v", err)
	}
	if tracePath == errPath {
		t.Fatalf("both channels wrote the same file: %q", tracePath)
	}

	traceData, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read render trace: %v", err)
	}
	errData, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatalf("read error log: %v", err)
	}
	if strings.TrimSpace(string(traceData)) != `{"label":"chat.state"}` {
		t.Fatalf("render trace content = %q", traceData)
	}
	if strings.TrimSpace(string(errData)) != `{"kind":"error"}` {
		t.Fatalf("error log content = %q", errData)
	}

	// A crossed sync.Once / field between the two lazy tracers would
	// surface here as a shared pointer.
	t1, err := app.frontendErrorLog()
	if err != nil {
		t.Fatalf("frontendErrorLog returned error: %v", err)
	}
	t2, err := app.frontendErrorLog()
	if err != nil {
		t.Fatalf("frontendErrorLog returned error: %v", err)
	}
	if t1 != t2 {
		t.Fatal("frontendErrorLog returned distinct Tracers across calls")
	}
	rt, err := app.uiTrace()
	if err != nil {
		t.Fatalf("uiTrace returned error: %v", err)
	}
	if rt == t1 {
		t.Fatal("uiTrace and frontendErrorLog returned the same Tracer")
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

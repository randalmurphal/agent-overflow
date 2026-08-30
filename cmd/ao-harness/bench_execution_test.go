package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBenchDocumentUsesCapturedTarget(t *testing.T) {
	root := t.TempDir()
	selected := target{DataDir: filepath.Join(root, "agent-overflow")}
	path, err := writeBenchDocument(benchDocument{Workload: "burst-stream"}, selected, "")
	if err != nil {
		t.Fatalf("writeBenchDocument: %v", err)
	}
	wantDir := filepath.Join(selected.DataDir, benchDirName)
	if !strings.HasPrefix(filepath.Clean(path), wantDir+string(filepath.Separator)) {
		t.Fatalf("report path = %q, want under captured target %q", path, wantDir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat report: %v", err)
	}
}

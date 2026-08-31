package workflowhost

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/safecopy"
)

func TestCaptureAndListArtifactsReplacesPriorExtensionAndSkipsTempFiles(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "report.txt"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	if err := CaptureArtifact(dataRoot, "item", "result", workspace, "report.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "report.md"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CaptureArtifact(dataRoot, "item", "result", workspace, "report.md"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ArtifactDir(dataRoot, "item"), safecopy.TempPrefix+"crash"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	listed, err := ListArtifacts(dataRoot, "item")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "result" || filepath.Ext(listed[0].Path) != ".md" {
		t.Fatalf("listed artifacts = %+v", listed)
	}
}

func TestArtifactPathsRejectEscapes(t *testing.T) {
	workspace := t.TempDir()
	dataRoot := t.TempDir()
	if err := CaptureArtifact(dataRoot, "item", "escape", workspace, "../report.txt"); err == nil {
		t.Fatal("escaping artifact path succeeded")
	}
	for _, itemID := range []string{"../item", ".", ".."} {
		if _, err := ListArtifacts(dataRoot, itemID); err == nil {
			t.Fatalf("item id %q unexpectedly listed", itemID)
		}
	}
}

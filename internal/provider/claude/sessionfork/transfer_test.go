package sessionfork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/transferfiles"
)

func TestTransferCarriesOpaqueSidecarsAcrossHomes(t *testing.T) {
	projects := filepath.Join(t.TempDir(), "projects")
	id := "4e4c615d-cd6e-4d51-8049-78a9ef5173a3"
	slug := "old-workspace"
	for name, body := range map[string]string{slug + "/" + id + ".jsonl": "native transcript\n", slug + "/" + id + "/subagents/agent-child.jsonl": "child transcript\n", slug + "/" + id + "/opaque/future-state.json": "{}", slug + "/unrelated.jsonl": "other session"} {
		file := filepath.Join(projects, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := TransferFiles(context.Background(), projects, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 3 {
		t.Fatalf("sources: %+v", sources)
	}
	archive := filepath.Join(t.TempDir(), "snapshot.tar")
	digest, err := transferfiles.Create(context.Background(), archive, sources)
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	stage := filepath.Join(t.TempDir(), "stage")
	files, err := transferfiles.Extract(context.Background(), in, digest, stage)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("extracted: %+v", files)
	}
	destinationProjects := filepath.Join(stage, "native", "claude")
	_, dest, err := RelocateSession(destinationProjects, id, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dest, destinationProjects+string(filepath.Separator)) {
		t.Fatalf("destination escaped injected home: %s", dest)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), id, "opaque", "future-state.json")); err != nil {
		t.Fatalf("lost opaque sidecar: %v", err)
	}
}

func TestTransferRefusesLinkedSidecarsAndMissingHome(t *testing.T) {
	if _, err := TransferFiles(context.Background(), "", "id", ""); err == nil {
		t.Fatal("defaulted provider home")
	}
	projects := t.TempDir()
	id := "4e4c615d-cd6e-4d51-8049-78a9ef5173a3"
	slug := filepath.Join(projects, "slug")
	if err := os.MkdirAll(filepath.Join(slug, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slug, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(slug, id, "external")); err != nil {
		t.Skip(err)
	}
	if _, err := TransferFiles(context.Background(), projects, id, ""); err == nil {
		t.Fatal("accepted incomplete linked sidecar tree")
	}
}

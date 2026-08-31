package harnessrpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness"
)

func TestHarnessSeedRefusesTraversalProjectNames(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	for _, name := range []string{"../outside", "a/b", `a\b`, ".", ".."} {
		_, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
			Name: name,
			Repo: &harness.RepoSpec{},
		}}})
		if err == nil || !strings.Contains(err.Error(), "plain directory name") {
			t.Fatalf("HarnessSeed(name=%q): err = %v, want plain-directory-name refusal", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(receiver.config.DataRoot, "outside")); !os.IsNotExist(err) {
		t.Fatalf("traversal seed escaped the workspaces root (stat err %v)", err)
	}
}

func TestHarnessSeedProviderHomeFilesWriteAndResetWipes(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	home := filepath.Join(receiver.config.DataRoot, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	receiver.config.CredentialHome = home
	gitconfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Seed(receiver, HarnessSeedSpec{ProviderHome: []HarnessSeedHomeFile{
		{Path: ".claude.json", Content: `{"mcpServers":{}}`},
		{Path: ".claude/projects/-tmp-x/abc.jsonl", Content: `{"type":"summary"}`},
	}})
	if err != nil {
		t.Fatalf("seed providerHome: %v", err)
	}
	if len(result.HomeFiles) != 2 {
		t.Fatalf("HomeFiles = %v, want 2 entries", result.HomeFiles)
	}
	for _, rel := range []string{".claude.json", ".claude/projects/-tmp-x/abc.jsonl"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("seeded home file %s: %v", rel, err)
		}
	}

	for _, bad := range []string{"", ".", "..", "../outside", "/etc/passwd", `C:\evil`, ".claude/../../out"} {
		if _, err := Seed(receiver, HarnessSeedSpec{ProviderHome: []HarnessSeedHomeFile{{Path: bad, Content: "x"}}}); err == nil {
			t.Fatalf("seed providerHome path %q: no error, want traversal refusal", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(receiver.config.DataRoot, "out")); !os.IsNotExist(err) {
		t.Fatalf("traversal seed escaped the home root (stat err %v)", err)
	}

	if err := receiver.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	for _, rel := range []string{".claude.json", ".claude"} {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Fatalf("reset left provider home state %s (stat err %v)", rel, err)
		}
	}
	if _, err := os.Stat(gitconfig); err != nil {
		t.Fatalf("reset removed .gitconfig: %v", err)
	}
}

func TestHarnessSeedWorkflowTargetValidationNamesTarget(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	_, err := receiver.seedWorkflowItems("project", HarnessSeedWorkflowItem{
		Workflow: "flow", Goal: "goal", Target: "queued",
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported target "queued"`) {
		t.Fatalf("unsupported target error = %v", err)
	}
}

func TestHarnessSeedWorkflowCountIsBoundedBeforeMutation(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	_, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "too-many",
		Repo: &harness.RepoSpec{},
		Workflows: &HarnessSeedWorkflows{Items: []HarnessSeedWorkflowItem{{
			Workflow: "flow", Goal: "goal", Count: maxHarnessSeedWorkflowItems + 1,
		}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "expanded item count exceeds") {
		t.Fatalf("oversized workflow seed error = %v", err)
	}
	projects, listErr := host.store.ListProjects()
	if listErr != nil || len(projects) != 0 {
		t.Fatalf("oversized seed mutated projects: %+v, %v", projects, listErr)
	}
}

package git

import (
	"errors"
	"strings"
	"testing"
)

func TestPRReferenceProject(t *testing.T) {
	cases := []struct {
		ref  PRReference
		want string
	}{
		{PRReference{Namespace: "owner", Repo: "repo"}, "owner/repo"},
		{PRReference{Namespace: "group/sub", Repo: "repo"}, "group/sub/repo"},
		{PRReference{Namespace: "", Repo: "repo"}, "repo"},
	}
	for _, tc := range cases {
		if got := tc.ref.Project(); got != tc.want {
			t.Errorf("Project() = %q, want %q", got, tc.want)
		}
	}
}

func TestSplitProjectForForge_GitHub(t *testing.T) {
	cases := []struct {
		input    string
		wantNS   string
		wantRepo string
	}{
		{"owner/repo", "owner", "repo"},
		{"  owner/repo  ", "owner", "repo"},
		{"owner/repo.name", "owner", "repo.name"},
		{"123-org/repo", "123-org", "repo"},
	}
	for _, tc := range cases {
		ns, repo, err := SplitProjectForForge("github", tc.input)
		if err != nil {
			t.Errorf("SplitProjectForForge(github, %q) error = %v", tc.input, err)
			continue
		}
		if ns != tc.wantNS || repo != tc.wantRepo {
			t.Errorf("SplitProjectForForge(github, %q) = (%q, %q), want (%q, %q)", tc.input, ns, repo, tc.wantNS, tc.wantRepo)
		}
	}
}

func TestSplitProjectForForge_GitLab(t *testing.T) {
	cases := []struct {
		input    string
		wantNS   string
		wantRepo string
	}{
		{"group/repo", "group", "repo"},
		{"group/sub/repo", "group/sub", "repo"},
		{"group/sub1/sub2/repo", "group/sub1/sub2", "repo"},
	}
	for _, tc := range cases {
		ns, repo, err := SplitProjectForForge("gitlab", tc.input)
		if err != nil {
			t.Errorf("SplitProjectForForge(gitlab, %q) error = %v", tc.input, err)
			continue
		}
		if ns != tc.wantNS || repo != tc.wantRepo {
			t.Errorf("SplitProjectForForge(gitlab, %q) = (%q, %q), want (%q, %q)", tc.input, ns, repo, tc.wantNS, tc.wantRepo)
		}
	}
}

func TestSplitProjectForForge_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		forge   string
		project string
		wantSub string
	}{
		{"empty", "github", "", "required"},
		{"whitespace only", "github", "   ", "required"},
		{"single segment github", "github", "owner", "OWNER/REPO"},
		{"single segment gitlab", "gitlab", "single", "NAMESPACE/REPO"},
		{"three segments github", "github", "a/b/c", "OWNER/REPO"},
		{"unsupported forge", "bitbucket", "a/b", "unsupported"},
		{"empty segment", "github", "owner//repo", "is empty"},
		{"dot segment", "gitlab", "group/./repo", "not allowed"},
		{"dotdot segment", "gitlab", "group/../repo", "not allowed"},
		{"leading dash", "github", "-flag/repo", "must not start"},
		{"control char", "github", "owner/repo\x00", "control or whitespace"},
		{"internal newline", "github", "own\ner/repo", "control or whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := SplitProjectForForge(tc.forge, tc.project)
			if err == nil {
				t.Fatalf("SplitProjectForForge(%q, %q) = nil, want error", tc.forge, tc.project)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateProjectSegment(t *testing.T) {
	good := []string{"owner", "repo.name", "123-org", "_x", "a"}
	for _, s := range good {
		if err := ValidateProjectSegment(s); err != nil {
			t.Errorf("ValidateProjectSegment(%q) = %v, want nil", s, err)
		}
	}

	bad := []string{"", ".", "..", "-flag", "a b", "a\x00", "a\t", "a\n", "a\x7f"}
	for _, s := range bad {
		if err := ValidateProjectSegment(s); err == nil {
			t.Errorf("ValidateProjectSegment(%q) = nil, want error", s)
		}
	}
}

func TestBuildPRAnchor(t *testing.T) {
	cases := []struct {
		forge, namespace, repo string
		want                   string
	}{
		{"github", "owner", "repo", "pr://github/owner/repo"},
		{"gitlab", "group/sub", "repo", "pr://gitlab/group/sub/repo"},
		{"gitlab", "group/sub1/sub2", "repo", "pr://gitlab/group/sub1/sub2/repo"},
	}
	for _, tc := range cases {
		got := BuildPRAnchor(tc.forge, tc.namespace, tc.repo)
		if got != tc.want {
			t.Errorf("BuildPRAnchor(%q, %q, %q) = %q, want %q", tc.forge, tc.namespace, tc.repo, got, tc.want)
		}
		if !strings.HasPrefix(got, PRAnchorScheme) {
			t.Errorf("anchor missing PRAnchorScheme prefix: %q", got)
		}
	}
}

func TestNormalizePRState(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"OPEN", "open"},
		{"open", "open"},
		{"opened", "open"},
		{"  Opened  ", "open"},
		{"CLOSED", "closed"},
		{"closed", "closed"},
		{"MERGED", "merged"},
		{"merged", "merged"},
		{"locked", "locked"},
		{"", ""},
		// Unknown values fall through to lowercased trimmed input —
		// callers branching on canonical values won't match, but raw
		// values aren't lost.
		{"WEIRD", "weird"},
	}
	for _, tc := range cases {
		if got := NormalizePRState(tc.input); got != tc.want {
			t.Errorf("NormalizePRState(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNullForgeReturnsErrUnsupported(t *testing.T) {
	f := nullForge{}

	if id := f.ID(); id != "" {
		t.Errorf("ID() = %q, want empty", id)
	}
	if bn := f.BinaryName(); bn != "" {
		t.Errorf("BinaryName() = %q, want empty", bn)
	}

	if _, err := f.ListOpenPRs("", "main"); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("ListOpenPRs err = %v, want ErrUnsupportedForge", err)
	}
	if _, err := f.CreatePR("", "title", "body", "", false); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("CreatePR err = %v, want ErrUnsupportedForge", err)
	}
	if _, err := f.ViewPR("", "owner/repo", 1); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("ViewPR err = %v, want ErrUnsupportedForge", err)
	}
	if _, err := f.Diff("", "owner/repo", 1); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("Diff err = %v, want ErrUnsupportedForge", err)
	}
}

func TestCoreForgeByID(t *testing.T) {
	core := NewCore()

	if got := core.ForgeByID("github").ID(); got != "github" {
		t.Errorf("ForgeByID(github).ID() = %q, want github", got)
	}
	if got := core.ForgeByID("gitlab").ID(); got != "gitlab" {
		t.Errorf("ForgeByID(gitlab).ID() = %q, want gitlab", got)
	}
	if got := core.ForgeByID("").ID(); got != "" {
		t.Errorf("ForgeByID(\"\").ID() = %q, want empty (nullForge)", got)
	}
	if got := core.ForgeByID("bitbucket").ID(); got != "" {
		t.Errorf("ForgeByID(bitbucket).ID() = %q, want empty (nullForge)", got)
	}
}

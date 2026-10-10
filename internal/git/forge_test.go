package git

import (
	"errors"
	"strings"
	"testing"
)

func TestPRReferenceProject(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestValidateProjectSegmentRejectsColon guards the injectivity of the PR
// entity key `<forge>:<project>:<number>`. A colon inside a segment lets two
// different pull requests spell one key — `github:a/b:c:1` is both
// (project "a/b:c", #1) and (project "a/b", #"c:1") — and that key is what
// every `pr:updated` frame is addressed by, so one PR's poll results would
// land on the other's panes. Neither forge allows a colon in a path segment.
func TestValidateProjectSegmentRejectsColon(t *testing.T) {
	t.Parallel()
	for _, seg := range []string{"a:b", ":", "repo:1", "own:er"} {
		if err := ValidateProjectSegment(seg); err == nil {
			t.Errorf("ValidateProjectSegment(%q) = nil, want a colon rejection", seg)
		}
	}
	// And through the parser the PR-reference path actually uses.
	for _, project := range []string{"owner/re:po", "grp:sub/repo"} {
		if _, _, err := SplitProjectForForge("gitlab", project); err == nil {
			t.Errorf("SplitProjectForForge(gitlab, %q) = nil, want a colon rejection", project)
		}
	}
}

func TestNormalizePRState(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f := nullForge{}

	if id := f.ID(); id != "" {
		t.Errorf("ID() = %q, want empty", id)
	}
	if bn := f.BinaryName(); bn != "" {
		t.Errorf("BinaryName() = %q, want empty", bn)
	}

	if _, err := f.ListOpenPRs(t.Context(), "", "main"); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("ListOpenPRs err = %v, want ErrUnsupportedForge", err)
	}
	if _, err := f.CreatePR(t.Context(), "", "title", "body", "", false); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("CreatePR err = %v, want ErrUnsupportedForge", err)
	}
	if _, err := f.ReadPR(t.Context(), testPRRef("owner/repo", 1), PRReadParts{Detail: true}, nil, nil); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("ReadPR err = %v, want ErrUnsupportedForge", err)
	}
}

func TestCoreForgeByID(t *testing.T) {
	t.Parallel()
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

// testPRRef is a reference for direct forge-implementation calls, which
// read only Project() and Number.
// testForgeHost is the host testPRRef puts on a reference: not either
// public host, so an argv that drops it fails the mock CLIs.
const testForgeHost = "forge.example"

func testPRRef(project string, number int) PRReference {
	slash := strings.LastIndex(project, "/")
	return PRReference{Host: testForgeHost, Namespace: project[:slash], Repo: project[slash+1:], Number: number}
}

func TestPRReferenceKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ref  PRReference
		want string
	}{
		// The public-host spelling is the one persisted draft sourceKeys
		// ("pr:" + key) were written under; it must never change.
		{PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 5}, "github:owner/repo:5"},
		{PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "group/sub", Repo: "repo", Number: 3}, "gitlab:group/sub/repo:3"},
		{PRReference{Forge: "github", Host: "ghe.example.com", Namespace: "owner", Repo: "repo", Number: 5}, "github@ghe.example.com:owner/repo:5"},
		{PRReference{Forge: "gitlab", Host: "gitlab.example.com:8443", Namespace: "group/sub", Repo: "repo", Number: 3}, "gitlab@gitlab.example.com:8443:group/sub/repo:3"},
		// The other forge's public host is not this forge's.
		{PRReference{Forge: "gitlab", Host: "github.com", Namespace: "group", Repo: "repo", Number: 1}, "gitlab@github.com:group/repo:1"},
	}
	for _, tc := range cases {
		if got := tc.ref.Key(); got != tc.want {
			t.Errorf("Key(%+v) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestPRReferenceValidate(t *testing.T) {
	t.Parallel()
	valid := PRReference{Forge: "gitlab", Host: "gitlab.example.com:8443", Namespace: "group/sub", Repo: "repo", Number: 3}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(%+v) = %v", valid, err)
	}
	if err := (PRReference{Forge: "github", Host: "[::1]:8443", Namespace: "o", Repo: "r", Number: 1}).Validate(); err != nil {
		t.Fatalf("an IPv6 literal host was refused: %v", err)
	}
	bad := map[string]PRReference{
		"empty host":      {Forge: "github", Namespace: "o", Repo: "r", Number: 1},
		"uppercase host":  {Forge: "github", Host: "GitHub.com", Namespace: "o", Repo: "r", Number: 1},
		"userinfo":        {Forge: "github", Host: "u@github.com", Namespace: "o", Repo: "r", Number: 1},
		"path in host":    {Forge: "github", Host: "github.com/x", Namespace: "o", Repo: "r", Number: 1},
		"space in host":   {Forge: "github", Host: "git hub.com", Namespace: "o", Repo: "r", Number: 1},
		"zero number":     {Forge: "github", Host: "github.com", Namespace: "o", Repo: "r"},
		"bad project":     {Forge: "github", Host: "github.com", Namespace: "a/b", Repo: "r", Number: 1},
		"colon segment":   {Forge: "gitlab", Host: "gitlab.com", Namespace: "g", Repo: "r:x", Number: 1},
		"unknown forge":   {Forge: "bitbucket", Host: "bitbucket.org", Namespace: "o", Repo: "r", Number: 1},
		"missing forge":   {Host: "github.com", Namespace: "o", Repo: "r", Number: 1},
		"control in host": {Forge: "github", Host: "github.com\x00", Namespace: "o", Repo: "r", Number: 1},
	}
	for name, ref := range bad {
		if err := ref.Validate(); err == nil {
			t.Errorf("%s: Validate(%+v) accepted it", name, ref)
		}
	}
	if err := bad["unknown forge"].Validate(); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("unknown forge error = %v, want ErrUnsupportedForge", err)
	}
}

// TestCoreRefusesInvalidPRReferenceBeforeDispatch proves the wrappers
// validate inside the API: a reference without a host never reaches a
// forge CLI, whichever wrapper the caller used.
func TestCoreRefusesInvalidPRReferenceBeforeDispatch(t *testing.T) {
	markers := installTrapCLIs(t)
	core := NewCore()
	ref := PRReference{Forge: "github", Namespace: "acme", Repo: "widgets", Number: 7}
	calls := map[string]func() error{
		"ReadPR": func() error {
			_, err := core.ReadPR(t.Context(), ref, PRReadParts{Detail: true, Threads: true, CI: true}, nil, nil)
			return err
		},
		"GetPRDetail": func() error { _, err := core.GetPRDetail(t.Context(), ref); return err },
		"ListReviewThreads": func() error {
			_, err := core.ListReviewThreads(t.Context(), ref)
			return err
		},
		"SubmitReview": func() error {
			_, err := core.SubmitReview(t.Context(), ref, SubmitReviewRequest{Verdict: ReviewVerdictComment})
			return err
		},
		"ReplyToThread":     func() error { return core.ReplyToThread(t.Context(), ref, "t", 1, "body") },
		"SetThreadResolved": func() error { return core.SetThreadResolved(t.Context(), ref, "t", true) },
		"ListPRCIJobs": func() error {
			_, err := core.ListPRCIJobs(t.Context(), ref, nil, nil)
			return err
		},
		"GetCIJobLog": func() error { _, err := core.GetCIJobLog(t.Context(), ref, CIJobLogRequest{JobID: "1"}); return err },
		"FetchAttachment": func() error {
			_, _, err := core.FetchAttachment(t.Context(), ref, "https://github.com/user-attachments/assets/0f1e2d3c", 1<<20)
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "host is required") {
			t.Errorf("%s error = %v, want the missing-host refusal", name, err)
		}
	}
	assertNoTrapRan(t, markers)
}

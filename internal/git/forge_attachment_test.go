package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/forgeattach"
	"agent-overflow/internal/testutil/mockexec"
)

// TestFetchAttachmentRefusesAMalformedHrefBeforeSpawning: the parse is
// the gate, and it runs before any subprocess or request exists.
func TestFetchAttachmentRefusesAMalformedHrefBeforeSpawning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock forge CLI is unix-only")
	}
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "spawned")
	mockexec.Write(t, filepath.Join(binDir, "glab"), fmt.Sprintf("#!/bin/sh\ntouch %q\n", marker))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer { return forgeUnexpected(t, call) })
	for _, tc := range []struct {
		ref  PRReference
		href string
	}{
		{PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widget", Number: 1}, "https://evil.example.com/user-attachments/assets/a"},
		{PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "g", Repo: "r", Number: 1}, "/uploads/short/a.png"},
		{PRReference{Forge: "bitbucket", Host: "bitbucket.org", Namespace: "g", Repo: "r", Number: 1}, "https://github.com/user-attachments/assets/a"},
	} {
		if _, _, err := core.FetchAttachment(t.Context(), tc.ref, tc.href, 1<<20); err == nil {
			t.Fatalf("FetchAttachment(%q) accepted a reference it cannot serve", tc.href)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a malformed reference spawned a forge CLI")
	}
	if n := len(calls.all()); n != 0 {
		t.Fatalf("a malformed reference sent %d forge requests", n)
	}
}

// TestFetchAttachmentIsUnsupportedForAnUnknownForge keeps the dispatch
// honest: a remote we do not integrate with answers ErrUnsupportedForge
// rather than reaching for a binary.
func TestFetchAttachmentIsUnsupportedForAnUnknownForge(t *testing.T) {
	t.Parallel()
	_, err := nullForge{}.FetchAttachment(t.Context(), PRReference{}, forgeattach.Target{}, 1<<20)
	if err != ErrUnsupportedForge {
		t.Fatalf("nullForge.FetchAttachment = %v, want ErrUnsupportedForge", err)
	}
}
func TestFetchAttachmentReportsAMissingBinary(t *testing.T) {
	t.Parallel()
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", TokenSource: missingTokenSource{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	core := NewCore(WithForgeAPI(svc))
	ref := PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widget", Number: 1}

	_, _, err = core.FetchAttachment(t.Context(), ref, "https://github.com/user-attachments/assets/abc", 1<<20)
	setup, ok := errors.AsType[*forgeapi.SetupError](err)
	if !ok {
		t.Fatalf("error is %T (%v), want *forgeapi.SetupError", err, err)
	}
	if setup.Kind != "missing" || setup.Binary != "gh" {
		t.Fatalf("setup error = %+v, want a missing gh", setup)
	}
}

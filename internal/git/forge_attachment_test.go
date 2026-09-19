package git

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/forgeattach"
)

const testUploadSecret = "0123456789abcdef0123456789abcdef"

// stubForgeCLI puts a shell script named binary on PATH and returns the
// file the script logs its argv to.
func stubForgeCLI(t *testing.T, binary, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock forge CLI is unix-only")
	}
	binDir := t.TempDir()
	argLog := filepath.Join(binDir, "args.log")
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\n%s", argLog, body)
	if err := os.WriteFile(filepath.Join(binDir, binary), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock %s: %v", binary, err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argLog
}

func readArgs(t *testing.T, argLog string) []string {
	t.Helper()
	raw, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// TestFetchAttachmentGitLabPassesTheUploadsEndpoint pins the exact argv:
// the REST path is the 17.4+ download-by-secret endpoint, and the project
// is URL-escaped into one path segment.
func TestFetchAttachmentGitLabPassesTheUploadsEndpoint(t *testing.T) {
	argLog := stubForgeCLI(t, "glab", "printf 'PNGBYTES'\n")
	core := NewCore()
	ref := PRReference{Forge: "gitlab", Namespace: "group/sub", Repo: "widget", Number: 4}

	data, filename, err := core.FetchAttachment("", ref, "/uploads/"+testUploadSecret+"/Screen%20Shot.png", 1<<20)
	if err != nil {
		t.Fatalf("FetchAttachment returned error: %v", err)
	}
	if string(data) != "PNGBYTES" {
		t.Fatalf("data = %q, want PNGBYTES", data)
	}
	if filename != "Screen Shot.png" {
		t.Fatalf("filename = %q, want %q", filename, "Screen Shot.png")
	}
	want := []string{"api", "projects/group%2Fsub%2Fwidget/uploads/" + testUploadSecret + "/Screen%20Shot.png"}
	if got := readArgs(t, argLog); !equalArgs(got, want) {
		t.Fatalf("glab argv = %q, want %q", got, want)
	}
}

// TestFetchAttachmentGitHubPassesTheURLAndEscapeFlag: gh uses an argument
// containing "://" as the request URL, so the absolute URL is what goes
// on the command line, with the escape-sequence opt-out a video needs.
func TestFetchAttachmentGitHubPassesTheURLAndEscapeFlag(t *testing.T) {
	argLog := stubForgeCLI(t, "gh", "printf 'MP4BYTES'\n")
	core := NewCore()
	ref := PRReference{Forge: "github", Namespace: "acme", Repo: "widget", Number: 9}
	const href = "https://github.com/user-attachments/assets/1f0c2c2e-1111"

	data, filename, err := core.FetchAttachment("", ref, href, 1<<20)
	if err != nil {
		t.Fatalf("FetchAttachment returned error: %v", err)
	}
	if string(data) != "MP4BYTES" {
		t.Fatalf("data = %q, want MP4BYTES", data)
	}
	if filename != "1f0c2c2e-1111" {
		t.Fatalf("filename = %q, want the asset id", filename)
	}
	want := []string{"api", href, "-H", "Accept: */*", "--allow-escape-sequences"}
	if got := readArgs(t, argLog); !equalArgs(got, want) {
		t.Fatalf("gh argv = %q, want %q", got, want)
	}
}

// TestFetchAttachmentIsByteTransparent: the body is media, so a NUL or an
// ESC in it is data. It reaches the caller unchanged, which is the whole
// reason --allow-escape-sequences is passed.
func TestFetchAttachmentIsByteTransparent(t *testing.T) {
	stubForgeCLI(t, "glab", `printf 'A\000B\033[31mC\377'`+"\n")
	core := NewCore()
	ref := PRReference{Forge: "gitlab", Namespace: "g", Repo: "r", Number: 1}

	data, _, err := core.FetchAttachment("", ref, "/uploads/"+testUploadSecret+"/a.bin", 1<<20)
	if err != nil {
		t.Fatalf("FetchAttachment returned error: %v", err)
	}
	want := []byte{'A', 0x00, 'B', 0x1b, '[', '3', '1', 'm', 'C', 0xff}
	if !bytes.Equal(data, want) {
		t.Fatalf("data = %v, want %v", data, want)
	}
}

// TestFetchAttachmentRefusesAnOverCapBody: the body is cut off and the
// error says what actually happened, rather than a truncated video being
// handed back as if it were whole.
func TestFetchAttachmentRefusesAnOverCapBody(t *testing.T) {
	stubForgeCLI(t, "glab", "head -c 4096 /dev/zero\n")
	core := NewCore()
	ref := PRReference{Forge: "gitlab", Namespace: "g", Repo: "r", Number: 1}

	_, _, err := core.FetchAttachment("", ref, "/uploads/"+testUploadSecret+"/big.bin", 1<<10)
	if err == nil {
		t.Fatal("FetchAttachment accepted a body over the cap")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error = %q, want it to say the attachment is too large", err)
	}
	// The message reaches the review pane, so it must not carry the
	// upload secret the argv did.
	if strings.Contains(err.Error(), testUploadSecret) {
		t.Fatalf("error %q leaks the upload secret", err)
	}
}

// TestFetchAttachmentReportsAForgeFailure: a non-zero exit becomes the
// CLI's own message, not "command failed" and not our argv.
func TestFetchAttachmentReportsAForgeFailure(t *testing.T) {
	stubForgeCLI(t, "gh", "echo 'gh: Not Found (HTTP 404)' >&2\nexit 1\n")
	core := NewCore()
	ref := PRReference{Forge: "github", Namespace: "acme", Repo: "widget", Number: 9}

	_, _, err := core.FetchAttachment("", ref, "https://github.com/user-attachments/assets/missing", 1<<20)
	if err == nil {
		t.Fatal("FetchAttachment ignored a non-zero exit")
	}
	const want = "gh api attachment download failed: gh: Not Found (HTTP 404)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

// TestFetchAttachmentGitLabFailureKeepsTheSetupShape: an unauthenticated
// glab is a typed setup problem the pane can act on, not a raw stderr line.
func TestFetchAttachmentGitLabFailureKeepsTheSetupShape(t *testing.T) {
	stubForgeCLI(t, "glab", "echo 'run glab auth login' >&2\nexit 1\n")
	core := NewCore()
	ref := PRReference{Forge: "gitlab", Namespace: "g", Repo: "r", Number: 1}

	_, _, err := core.FetchAttachment("", ref, "/uploads/"+testUploadSecret+"/a.png", 1<<20)
	if err == nil {
		t.Fatal("FetchAttachment ignored a non-zero exit")
	}
	setup, ok := err.(*ForgeSetupError)
	if !ok {
		t.Fatalf("error is %T (%v), want *ForgeSetupError", err, err)
	}
	if setup.Kind != "unauthenticated" || setup.Binary != "glab" {
		t.Fatalf("setup error = %+v, want an unauthenticated glab", setup)
	}
}

// TestFetchAttachmentRetriesWithoutTheEscapeFlag: an older gh refuses the
// flag outright, and losing every video on that version would be a worse
// answer than one extra process.
func TestFetchAttachmentRetriesWithoutTheEscapeFlag(t *testing.T) {
	argLog := stubForgeCLI(t, "gh", `
for a in "$@"; do
  if [ "$a" = "--allow-escape-sequences" ]; then
    echo 'unknown flag: --allow-escape-sequences' >&2
    exit 1
  fi
done
printf 'OLDGHBYTES'
`)
	core := NewCore()
	ref := PRReference{Forge: "github", Namespace: "acme", Repo: "widget", Number: 9}
	const href = "https://github.com/user-attachments/assets/abc"

	data, _, err := core.FetchAttachment("", ref, href, 1<<20)
	if err != nil {
		t.Fatalf("FetchAttachment returned error: %v", err)
	}
	if string(data) != "OLDGHBYTES" {
		t.Fatalf("data = %q, want OLDGHBYTES", data)
	}
	want := []string{
		"api", href, "-H", "Accept: */*", "--allow-escape-sequences",
		"api", href, "-H", "Accept: */*",
	}
	if got := readArgs(t, argLog); !equalArgs(got, want) {
		t.Fatalf("gh argv across both runs = %q, want %q", got, want)
	}
}

// TestFetchAttachmentRefusesAMalformedHrefBeforeSpawning: the parse is
// the gate, and it runs before any subprocess exists.
func TestFetchAttachmentRefusesAMalformedHrefBeforeSpawning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock forge CLI is unix-only")
	}
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "spawned")
	for _, binary := range []string{"gh", "glab"} {
		script := fmt.Sprintf("#!/bin/sh\ntouch %q\n", marker)
		if err := os.WriteFile(filepath.Join(binDir, binary), []byte(script), 0o755); err != nil {
			t.Fatalf("write mock %s: %v", binary, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	for _, tc := range []struct {
		ref  PRReference
		href string
	}{
		{PRReference{Forge: "github", Namespace: "acme", Repo: "widget", Number: 1}, "https://evil.example.com/user-attachments/assets/a"},
		{PRReference{Forge: "gitlab", Namespace: "g", Repo: "r", Number: 1}, "/uploads/short/a.png"},
		{PRReference{Forge: "bitbucket", Namespace: "g", Repo: "r", Number: 1}, "https://github.com/user-attachments/assets/a"},
	} {
		if _, _, err := core.FetchAttachment("", tc.ref, tc.href, 1<<20); err == nil {
			t.Fatalf("FetchAttachment(%q) accepted a reference it cannot serve", tc.href)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a malformed reference spawned a forge CLI")
	}
}

// TestFetchAttachmentIsUnsupportedForAnUnknownForge keeps the dispatch
// honest: a remote we do not integrate with answers ErrUnsupportedForge
// rather than reaching for a binary.
func TestFetchAttachmentIsUnsupportedForAnUnknownForge(t *testing.T) {
	_, err := nullForge{}.FetchAttachment("", forgeattach.Target{}, 1<<20)
	if err != ErrUnsupportedForge {
		t.Fatalf("nullForge.FetchAttachment = %v, want ErrUnsupportedForge", err)
	}
}

func equalArgs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestRedactForgeRequest: a run that never produced an exit status (a
// timeout, a child killed at shutdown) reports through the shared
// runner, whose message names the command it ran — and that name is the
// GitLab upload secret or a signed GitHub URL.
func TestRedactForgeRequest(t *testing.T) {
	args := []string{"api", "projects/g%2Fr/uploads/" + testUploadSecret + "/a.png"}
	message := formatCommand("glab", args...) + " timed out after 10m0s"
	redacted := redactForgeRequest(message, args)
	if strings.Contains(redacted, testUploadSecret) {
		t.Fatalf("redacted message %q still carries the upload secret", redacted)
	}
	if redacted != "glab api <attachment> timed out after 10m0s" {
		t.Fatalf("redacted message = %q", redacted)
	}

	// A quoted argument is the same value spelled differently, and the
	// runner quotes anything containing a space.
	ghArgs := []string{"api", "https://github.com/user-attachments/assets/abc", "-H", "Accept: */*"}
	ghMessage := formatCommand("gh", ghArgs...) + " cancelled"
	ghRedacted := redactForgeRequest(ghMessage, ghArgs)
	if strings.Contains(ghRedacted, "github.com") || strings.Contains(ghRedacted, "Accept") {
		t.Fatalf("redacted message %q still carries the request", ghRedacted)
	}

	// Short arguments are left alone: they are flags, not requests, and
	// eliding them would make the message unreadable.
	if got := redactForgeRequest("gh api x -H y failed", []string{"api", "x", "-H", "y"}); got != "gh api x -H y failed" {
		t.Fatalf("redacted short arguments: %q", got)
	}
}

// TestFetchAttachmentReportsAMissingBinary: the redaction wrapper keeps
// the cause, so a CLI that is not installed is still the typed setup
// error the pane knows how to act on.
func TestFetchAttachmentReportsAMissingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH shape is unix-only")
	}
	t.Setenv("PATH", t.TempDir())
	core := NewCore()
	ref := PRReference{Forge: "github", Namespace: "acme", Repo: "widget", Number: 1}

	_, _, err := core.FetchAttachment("", ref, "https://github.com/user-attachments/assets/abc", 1<<20)
	setup, ok := err.(*ForgeSetupError)
	if !ok {
		t.Fatalf("error is %T (%v), want *ForgeSetupError", err, err)
	}
	if setup.Kind != "missing" || setup.Binary != "gh" {
		t.Fatalf("setup error = %+v, want a missing gh", setup)
	}
}

package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalFilePathForPageURLAcceptsOnlyLocalFiles(t *testing.T) {
	got, err := localFilePathForPageURL("file:///repo/docs/My%20Report.pdf")
	if err != nil || got != filepath.FromSlash("/repo/docs/My Report.pdf") {
		t.Fatalf("file URL = %q, %v, want the percent-decoded path", got, err)
	}
	for _, tc := range []struct{ name, url string }{
		{"remote", "https://example.test/report.pdf"},
		{"blank", "about:blank"},
		{"empty", "   "},
		{"schemeless", "/repo/docs/report.pdf"},
	} {
		if _, err := localFilePathForPageURL(tc.url); err == nil {
			t.Fatalf("%s: %q was accepted, want a refusal naming the limitation", tc.name, tc.url)
		}
	}
	if _, err := localFilePathForPageURL("https://example.test/a.pdf"); err == nil ||
		!strings.Contains(err.Error(), "only local files") {
		t.Fatalf("remote refusal = %v, want it to say only local files can be copied", err)
	}
}

func TestCopyPageFileResolvesTheDisplayedFileAndRefusesTheRest(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(real, []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "latest.pdf")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	p := &managedPage{id: "page", owner: "thread", ctx: context.Background()}
	var copied string
	m := &Manager{
		scopes:   map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{p.id: p}}},
		sessions: map[string]SessionInfo{"thread": {ActivePageID: p.id}},
		copyFileToOSClipboard: func(_ context.Context, path string) error {
			copied = path
			return nil
		},
	}
	access := Access{ThreadID: "thread", Workspace: "/repo"}

	// A symlinked address resolves to the real file: the clipboard must carry
	// bytes that outlive the link.
	p.setInfo(PageInfo{ID: p.id, URL: "file://" + filepath.ToSlash(link)})
	if err := m.CopyPageFileToClipboard(context.Background(), access, p.id); err != nil {
		t.Fatalf("copy = %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(real); copied != resolved {
		t.Fatalf("copied %q, want the resolved %q", copied, resolved)
	}

	copied = ""
	p.setInfo(PageInfo{ID: p.id, URL: "https://example.test/report.pdf"})
	if err := m.CopyPageFileToClipboard(context.Background(), access, p.id); err == nil {
		t.Fatal("a remote page was copied")
	}
	p.setInfo(PageInfo{ID: p.id, URL: "file://" + filepath.ToSlash(dir)})
	if err := m.CopyPageFileToClipboard(context.Background(), access, p.id); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory copy = %v, want a regular-file refusal", err)
	}
	if copied != "" {
		t.Fatalf("a refused copy still reached the clipboard: %q", copied)
	}
}

// rendererFSEngine is a stub whose renderer addresses files on a different
// machine: URLs carry the host "renderer.host" and map onto base.
type rendererFSEngine struct {
	browserEngine
	base string
}

func (e rendererFSEngine) FileURL(_ context.Context, path string) (string, error) {
	return "file://renderer.host" + filepath.ToSlash(strings.TrimPrefix(path, e.base)), nil
}

func (e rendererFSEngine) BackendFilePath(_ context.Context, rawURL string) (string, error) {
	rest, ok := strings.CutPrefix(rawURL, "file://renderer.host/")
	if !ok {
		return "", context.Canceled
	}
	return filepath.Join(e.base, filepath.FromSlash(rest)), nil
}

// A page's address on the hosted engine is in the RENDERER's form; the
// clipboard copy must resolve the backend path through the engine seam
// rather than stat the URL's path (live incident 2026-08-31).
func TestCopyPageFileResolvesRendererFormURLsThroughTheEngine(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(real, []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &managedPage{id: "page", owner: "thread", ctx: context.Background()}
	var copied string
	m := &Manager{
		engine:   rendererFSEngine{base: dir},
		scopes:   map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{p.id: p}}},
		sessions: map[string]SessionInfo{"thread": {ActivePageID: p.id}},
		copyFileToOSClipboard: func(_ context.Context, path string) error {
			copied = path
			return nil
		},
	}
	access := Access{ThreadID: "thread", Workspace: "/repo"}
	p.setInfo(PageInfo{ID: p.id, URL: "file://renderer.host/report.pdf"})
	if err := m.CopyPageFileToClipboard(context.Background(), access, p.id); err != nil {
		t.Fatalf("copy = %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(real); copied != resolved {
		t.Fatalf("copied %q, want the seam-resolved %q", copied, resolved)
	}
}

func TestWindowsClipboardCommandQuotesForPowerShell(t *testing.T) {
	command, err := windowsClipboardCommand(`C:\Users\r\AppData\Local\Temp\agent-overflow-clipboard\it's [1].pdf`)
	if err != nil {
		t.Fatalf("command = %v", err)
	}
	if command.Name != "powershell.exe" {
		t.Fatalf("command name = %q", command.Name)
	}
	script := command.Args[len(command.Args)-1]
	// A doubled quote is PowerShell's whole escaping rule for a single-quoted
	// literal, and -LiteralPath is what keeps `[` from globbing.
	if !strings.Contains(script, `Set-Clipboard -LiteralPath '`) || !strings.Contains(script, `it''s [1].pdf`) {
		t.Fatalf("script = %q", script)
	}
	if _, err := windowsClipboardCommand("C:\\a\nb"); err == nil {
		t.Fatal("a path with a line break was accepted")
	}
	if _, err := windowsClipboardCommand("  "); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

func TestMacClipboardCommandRefusesWhatAppleScriptCannotQuote(t *testing.T) {
	command, err := macClipboardCommand("/Users/r/Documents/report.pdf")
	if err != nil {
		t.Fatalf("command = %v", err)
	}
	if command.Name != "osascript" || len(command.Args) != 2 || command.Args[0] != "-e" {
		t.Fatalf("command = %#v", command)
	}
	if command.Args[1] != `set the clipboard to (POSIX file "/Users/r/Documents/report.pdf")` {
		t.Fatalf("script = %q", command.Args[1])
	}
	for _, path := range []string{`/tmp/a"b.pdf`, `/tmp/a\b.pdf`, "/tmp/a\nb.pdf"} {
		if _, err := macClipboardCommand(path); err == nil {
			t.Fatalf("%q was accepted rather than refused", path)
		}
	}
}

func TestLinuxClipboardCommandsOfferAFileURIToBothServers(t *testing.T) {
	commands := linuxClipboardCommands("/home/r/My Docs/report.pdf")
	if len(commands) != 2 || commands[0].Name != "wl-copy" || commands[1].Name != "xclip" {
		t.Fatalf("candidates = %#v, want Wayland then X11", commands)
	}
	for _, command := range commands {
		if !strings.Contains(strings.Join(command.Args, " "), "text/uri-list") {
			t.Fatalf("%s args = %v, want the uri-list target", command.Name, command.Args)
		}
		if command.Stdin != "file:///home/r/My%20Docs/report.pdf\n" {
			t.Fatalf("%s stdin = %q, want a percent-encoded file URI", command.Name, command.Stdin)
		}
	}
}

func TestStageClipboardFileCopiesUnderItsOwnNameAndLeavesNoTemp(t *testing.T) {
	source := filepath.Join(t.TempDir(), "quarterly report.pdf")
	if err := os.WriteFile(source, []byte("%PDF-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), clipboardStagingDir)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}

	staged, err := stageClipboardFile(source, staging)
	if err != nil {
		t.Fatalf("stage = %v", err)
	}
	if staged != filepath.Join(staging, "quarterly report.pdf") {
		t.Fatalf("staged at %q", staged)
	}
	body, err := os.ReadFile(staged)
	if err != nil || string(body) != "%PDF-bytes" {
		t.Fatalf("staged bytes = %q, %v", body, err)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("staging directory = %v, want only the staged copy", entries)
	}

	// Copying the same page twice must replace the previous staging copy
	// rather than accumulate one file per attempt.
	if _, err := stageClipboardFile(source, staging); err != nil {
		t.Fatalf("restage = %v", err)
	}
	entries, _ = os.ReadDir(staging)
	if len(entries) != 1 {
		t.Fatalf("restaging left %v", entries)
	}
}

func TestParseWindowsTempOutputIgnoresTheUNCWarningAndUnexpandedVars(t *testing.T) {
	// cmd.exe launched from a Linux working directory prints this preamble on
	// stdout before the answer.
	output := "'\\\\wsl.localhost\\Distro\\home\\r'\r\n" +
		"CMD.EXE was started with the above path as the current directory.\r\n" +
		"UNC paths are not supported.  Defaulting to Windows directory.\r\n" +
		"C:\\Users\\r\\AppData\\Local\\Temp\\\r\n"
	if got := parseWindowsTempOutput(output); got != `C:\Users\r\AppData\Local\Temp` {
		t.Fatalf("temp dir = %q", got)
	}
	if got := parseWindowsTempOutput("%TEMP%\r\n"); got != "" {
		t.Fatalf("unexpanded variable = %q, want no answer", got)
	}
	if got := parseWindowsTempOutput("  \r\n\r\n"); got != "" {
		t.Fatalf("empty output = %q", got)
	}
}
